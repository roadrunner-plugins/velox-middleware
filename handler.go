package veloxmiddleware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/roadrunner-server/api/v4/plugins/v4/middleware"
	"go.uber.org/zap"
)

// handleVeloxBuild processes a Velox build request intercepted from PHP worker response.
func (p *Plugin) handleVeloxBuild(req middleware.Request, resp middleware.Response) error {
	startTime := time.Now()

	// Parse build request from response body
	buildReq, err := p.parseBuildRequest(resp.Body())
	if err != nil {
		p.log.Error("failed to parse build request",
			zap.Error(err),
			zap.String("remote_addr", req.RemoteAddr()),
		)
		return p.writeErrorResponse(resp, http.StatusBadRequest, "invalid_request", err.Error(), "")
	}

	p.log.Info("build request received",
		zap.String("request_id", buildReq.RequestID),
		zap.String("os", buildReq.TargetPlatform.OS),
		zap.String("arch", buildReq.TargetPlatform.Arch),
		zap.String("rr_version", buildReq.RRVersion),
		zap.Int("plugins_count", len(buildReq.Plugins)),
		zap.Bool("force_rebuild", buildReq.ForceRebuild),
	)

	// Generate cache key
	cacheKey := buildReq.GenerateCacheKey()

	// Check cache (unless force rebuild)
	if !buildReq.ForceRebuild && p.cache != nil {
		if entry, found := p.cache.Get(cacheKey); found {
			p.log.Info("cache hit",
				zap.String("request_id", buildReq.RequestID),
				zap.String("cache_key", cacheKey),
				zap.Duration("cache_age", time.Since(entry.CreatedAt)),
				zap.Int64("file_size", entry.FileSize),
			)

			// Stream cached binary
			if err := p.streamCachedBinary(resp, buildReq, entry); err != nil {
				p.log.Error("failed to stream cached binary",
					zap.Error(err),
					zap.String("request_id", buildReq.RequestID),
					zap.String("cache_key", cacheKey),
				)
				return p.writeErrorResponse(resp, http.StatusInternalServerError, "cache_read_error", "Failed to read cached binary", buildReq.RequestID)
			}

			metricsIncCacheHits()
			metricsObserveStreamTTFB(time.Since(startTime), "hit")

			return nil
		}

		p.log.Info("cache miss, building from Velox server",
			zap.String("request_id", buildReq.RequestID),
			zap.String("cache_key", cacheKey),
		)
		metricsIncCacheMisses()
	} else if buildReq.ForceRebuild {
		p.log.Info("force rebuild requested, skipping cache",
			zap.String("request_id", buildReq.RequestID),
			zap.String("cache_key", cacheKey),
		)
		metricsIncForceRebuilds()
	}

	// Acquire semaphore slot for build
	if !p.sem.tryAcquire() {
		// Wait for available slot or context cancellation
		select {
		case <-p.sem.acquire():
			defer p.sem.release()
		case <-req.Context().Done():
			p.log.Warn("client disconnected while waiting for build slot",
				zap.String("request_id", buildReq.RequestID),
			)
			metricsIncClientDisconnects("queue")
			return req.Context().Err()
		}
	} else {
		defer p.sem.release()
	}

	metricsSetActiveBuilds(p.sem.active())

	// Build with timeout
	buildCtx, buildCancel := context.WithTimeout(req.Context(), p.cfg.BuildTimeout)
	defer buildCancel()

	// Execute build and stream result
	if err := p.buildAndStream(buildCtx, resp, buildReq, cacheKey, startTime); err != nil {
		p.log.Error("build failed",
			zap.Error(err),
			zap.String("request_id", buildReq.RequestID),
			zap.String("cache_key", cacheKey),
		)

		// Determine error type and status code
		statusCode := http.StatusBadGateway
		errorType := "build_failed"

		if buildCtx.Err() == context.DeadlineExceeded {
			statusCode = http.StatusGatewayTimeout
			errorType = "timeout"
			metricsIncBuildsByStatus("timeout", "miss")
		} else {
			metricsIncBuildsByStatus("error", "miss")
		}

		return p.writeErrorResponse(resp, statusCode, errorType, err.Error(), buildReq.RequestID)
	}

	metricsSetActiveBuilds(p.sem.active())
	metricsIncBuildsByStatus("success", "miss")
	metricsObserveStreamTTFB(time.Since(startTime), "miss")

	return nil
}

// parseBuildRequest parses the build request from the response body.
func (p *Plugin) parseBuildRequest(body []byte) (*BuildRequest, error) {
	var req BuildRequest

	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("failed to parse JSON: %w", err)
	}

	if err := req.Validate(); err != nil {
		return nil, fmt.Errorf("validation failed: %w", err)
	}

	return &req, nil
}

// streamCachedBinary streams a cached binary to the client.
func (p *Plugin) streamCachedBinary(resp middleware.Response, req *BuildRequest, entry *CacheEntry) error {
	// Open cached file
	file, err := p.cache.OpenBinary(entry.FilePath)
	if err != nil {
		return fmt.Errorf("failed to open cached file: %w", err)
	}
	defer file.Close()

	// Set response headers
	resp.Header().Set("Content-Type", "application/octet-stream")
	resp.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", req.BinaryFilename()))
	resp.Header().Set("Content-Length", fmt.Sprintf("%d", entry.FileSize))
	resp.Header().Set("X-Build-Request-ID", req.RequestID)
	resp.Header().Set("X-Cache-Status", "HIT")
	resp.Header().Set("X-Cache-Age", fmt.Sprintf("%d", int(time.Since(entry.CreatedAt).Seconds())))

	resp.WriteHeader(http.StatusOK)

	// Stream file to client using buffered pool
	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	if _, err := io.CopyBuffer(resp.Body(), file, buf); err != nil {
		return fmt.Errorf("failed to stream cached binary: %w", err)
	}

	// Update access time in background (non-blocking)
	go p.cache.UpdateAccessTime(entry.Key)

	return nil
}

// buildAndStream executes a Velox build and streams the result to the client.
func (p *Plugin) buildAndStream(ctx context.Context, resp middleware.Response, req *BuildRequest, cacheKey string, startTime time.Time) error {
	buildStart := time.Now()

	// Call Velox server
	veloxResp, err := p.client.Build(ctx, req)
	if err != nil {
		metricsIncErrors("velox_server")
		return fmt.Errorf("velox build failed: %w", err)
	}
	defer veloxResp.Body.Close()

	// Check response status
	if veloxResp.StatusCode != http.StatusOK {
		metricsIncErrors("velox_server")
		body, _ := io.ReadAll(veloxResp.Body)
		return fmt.Errorf("velox server returned status %d: %s", veloxResp.StatusCode, string(body))
	}

	// Prepare cache file (if caching enabled)
	var cacheWriter io.WriteCloser
	var cachePath string

	if p.cache != nil {
		cacheWriter, cachePath, err = p.cache.PrepareWrite(cacheKey)
		if err != nil {
			p.log.Warn("failed to prepare cache write, continuing without caching",
				zap.Error(err),
				zap.String("request_id", req.RequestID),
			)
		}
		defer func() {
			if cacheWriter != nil {
				cacheWriter.Close()
			}
		}()
	}

	// Set response headers
	resp.Header().Set("Content-Type", "application/octet-stream")
	resp.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", req.BinaryFilename()))
	resp.Header().Set("X-Build-Request-ID", req.RequestID)
	resp.Header().Set("X-Cache-Status", "MISS")

	// Get content length if available
	if veloxResp.ContentLength > 0 {
		resp.Header().Set("Content-Length", fmt.Sprintf("%d", veloxResp.ContentLength))
	}

	resp.WriteHeader(http.StatusOK)

	// Stream binary to client and cache simultaneously
	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	var writers []io.Writer
	writers = append(writers, resp.Body())
	if cacheWriter != nil {
		writers = append(writers, cacheWriter)
	}

	multiWriter := io.MultiWriter(writers...)
	bytesWritten, err := io.CopyBuffer(multiWriter, veloxResp.Body, buf)
	if err != nil {
		if cacheWriter != nil {
			// Delete incomplete cache file
			p.cache.CleanupFailedWrite(cachePath)
		}
		metricsIncErrors("stream")
		return fmt.Errorf("failed to stream binary: %w", err)
	}

	buildDuration := time.Since(buildStart)
	resp.Header().Set("X-Build-Time", fmt.Sprintf("%.1f", buildDuration.Seconds()))

	p.log.Info("build completed",
		zap.String("request_id", req.RequestID),
		zap.Duration("build_duration", buildDuration),
		zap.Int64("bytes_written", bytesWritten),
	)

	// Finalize cache entry
	if cacheWriter != nil && p.cache != nil {
		if err := cacheWriter.Close(); err != nil {
			p.log.Warn("failed to close cache writer",
				zap.Error(err),
				zap.String("request_id", req.RequestID),
			)
			p.cache.CleanupFailedWrite(cachePath)
			metricsIncErrors("cache_write")
		} else {
			// Commit cache entry
			entry := &CacheEntry{
				Key:          cacheKey,
				FilePath:     cachePath,
				FileSize:     bytesWritten,
				CreatedAt:    time.Now(),
				ExpiresAt:    time.Now().Add(p.cfg.Cache.TTL()),
				AccessedAt:   time.Now(),
				BuildRequest: *req,
			}

			if err := p.cache.CommitWrite(entry); err != nil {
				p.log.Warn("failed to commit cache entry",
					zap.Error(err),
					zap.String("request_id", req.RequestID),
				)
				metricsIncErrors("cache_write")
			} else {
				p.log.Info("binary cached",
					zap.String("request_id", req.RequestID),
					zap.String("cache_key", cacheKey),
					zap.Int64("file_size", bytesWritten),
				)
			}
		}
	}

	metricsObserveBuildDuration(buildDuration, req.TargetPlatform.OS, req.TargetPlatform.Arch)
	metricsObserveBinarySize(bytesWritten, req.TargetPlatform.OS, req.TargetPlatform.Arch)

	return nil
}

// writeErrorResponse writes an error response to the client.
func (p *Plugin) writeErrorResponse(resp middleware.Response, statusCode int, errorType, message, requestID string) error {
	resp.Header().Set("Content-Type", "application/json")
	resp.WriteHeader(statusCode)

	errorResp := map[string]string{
		"error":   errorType,
		"message": message,
	}

	if requestID != "" {
		errorResp["request_id"] = requestID
	}

	return json.NewEncoder(resp.Body()).Encode(errorResp)
}
