package veloxmiddleware

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// handleVeloxBuild processes a Velox build request intercepted from the PHP worker response.
func (p *Plugin) handleVeloxBuild(w http.ResponseWriter, r *http.Request, body []byte) {
	startTime := time.Now()

	// Parse build request from PHP worker response body
	buildReq, err := p.parseBuildRequest(body)
	if err != nil {
		p.log.Error("failed to parse build request",
			zap.Error(err),
			zap.String("remote_addr", r.RemoteAddr),
		)
		p.writeErrorResponse(w, http.StatusBadRequest, "invalid_request", err.Error(), "")
		return
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
			if err := p.streamCachedBinary(w, buildReq, entry); err != nil {
				p.log.Error("failed to stream cached binary",
					zap.Error(err),
					zap.String("request_id", buildReq.RequestID),
					zap.String("cache_key", cacheKey),
				)
				p.writeErrorResponse(w, http.StatusInternalServerError, "cache_read_error", "Failed to read cached binary", buildReq.RequestID)
			}

			metricsIncCacheHits()
			metricsObserveStreamTTFB(time.Since(startTime), "hit")

			return
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
		case <-r.Context().Done():
			p.log.Warn("client disconnected while waiting for build slot",
				zap.String("request_id", buildReq.RequestID),
			)
			metricsIncClientDisconnects("queue")
			return
		}
	} else {
		defer p.sem.release()
	}

	metricsSetActiveBuilds(p.sem.active())

	// Build with timeout
	buildCtx, buildCancel := context.WithTimeout(r.Context(), p.cfg.BuildTimeout)
	defer buildCancel()

	// Execute build and stream result
	if err := p.buildAndStream(buildCtx, w, buildReq, cacheKey, startTime); err != nil {
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

		p.writeErrorResponse(w, statusCode, errorType, err.Error(), buildReq.RequestID)
		return
	}

	metricsSetActiveBuilds(p.sem.active())
	metricsIncBuildsByStatus("success", "miss")
	metricsObserveStreamTTFB(time.Since(startTime), "miss")
}

// parseBuildRequest parses the build request from the request body.
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
func (p *Plugin) streamCachedBinary(w http.ResponseWriter, req *BuildRequest, entry *CacheEntry) error {
	// Open cached file
	file, err := p.cache.OpenBinary(entry.FilePath)
	if err != nil {
		return fmt.Errorf("failed to open cached file: %w", err)
	}
	defer file.Close()

	// Set response headers
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", req.BinaryFilename()))
	w.Header().Set("Content-Length", fmt.Sprintf("%d", entry.FileSize))
	w.Header().Set("X-Build-Request-ID", req.RequestID)
	w.Header().Set("X-Cache-Status", "HIT")
	w.Header().Set("X-Cache-Age", fmt.Sprintf("%d", int(time.Since(entry.CreatedAt).Seconds())))

	w.WriteHeader(http.StatusOK)

	// Stream file to client using buffered pool
	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	if _, err := io.CopyBuffer(w, file, buf); err != nil {
		return fmt.Errorf("failed to stream cached binary: %w", err)
	}

	// Update access time in background (non-blocking)
	go p.cache.UpdateAccessTime(entry.Key)

	return nil
}

// buildAndStream executes a Velox build and streams the result to the client.
func (p *Plugin) buildAndStream(ctx context.Context, w http.ResponseWriter, req *BuildRequest, cacheKey string, startTime time.Time) error {
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
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", req.BinaryFilename()))
	w.Header().Set("X-Build-Request-ID", req.RequestID)
	w.Header().Set("X-Cache-Status", "MISS")

	// Get content length if available
	if veloxResp.ContentLength > 0 {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", veloxResp.ContentLength))
	}

	w.WriteHeader(http.StatusOK)

	// Stream binary to client and cache simultaneously
	buf := bufferPool.Get().([]byte)
	defer bufferPool.Put(buf)

	var writers []io.Writer
	writers = append(writers, w)
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
	w.Header().Set("X-Build-Time", fmt.Sprintf("%.1f", buildDuration.Seconds()))

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
func (p *Plugin) writeErrorResponse(w http.ResponseWriter, statusCode int, errorType, message, requestID string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)

	errorResp := map[string]string{
		"error":   errorType,
		"message": message,
	}

	if requestID != "" {
		errorResp["request_id"] = requestID
	}

	json.NewEncoder(w).Encode(errorResp)
}
