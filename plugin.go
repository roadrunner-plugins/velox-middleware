package velox

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	rrerrors "github.com/roadrunner-server/errors"
	"go.uber.org/zap"
)

const (
	// PluginName is the name of the plugin
	PluginName = "velox"

	// xVeloxBuildHeader is the header that indicates a Velox build request
	xVeloxBuildHeader = "X-Velox-Build"

	// ContentTypeJSON is the expected content type for build requests
	ContentTypeJSON = "application/json"

	// ContentTypeOctetStream is the content type for binary responses
	ContentTypeOctetStream = "application/octet-stream"

	// buildEndpoint is the Velox build service endpoint path
	buildEndpoint = "/api.service.v1.BuildService/Build"

	// bufSize is the buffer size for file streaming (10MB)
	bufSize = 10 * 1024 * 1024
)

// Logger interface for structured logging
type Logger interface {
	NamedLogger(name string) *zap.Logger
}

// Configurer provides access to plugin configuration
type Configurer interface {
	UnmarshalKey(name string, out any) error
	Has(name string) bool
}

// Plugin is the Velox middleware plugin
type Plugin struct {
	log         *zap.Logger
	cfg         *Config
	httpClient  *http.Client
	writersPool sync.Pool
}

// Init initializes the plugin with configuration and dependencies
func (p *Plugin) Init(cfg Configurer, log Logger) error {
	const op = rrerrors.Op("velox_plugin_init")

	if !cfg.Has(PluginName) {
		return rrerrors.E(op, rrerrors.Disabled)
	}

	// Initialize logger
	p.log = log.NamedLogger(PluginName)

	// Parse configuration
	p.cfg = &Config{}
	if err := cfg.UnmarshalKey(PluginName, p.cfg); err != nil {
		return rrerrors.E(op, err)
	}

	// Set defaults
	p.cfg.InitDefaults()

	// Validate configuration
	if err := p.cfg.Validate(); err != nil {
		return rrerrors.E(op, err)
	}

	// Initialize HTTP client
	p.httpClient = &http.Client{
		Timeout: p.cfg.RequestTimeout,
		Transport: &http.Transport{
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 10,
			IdleConnTimeout:     90 * time.Second,
		},
	}

	// Initialize writer pool
	p.writersPool = sync.Pool{
		New: func() any {
			wr := new(writer)
			wr.code = http.StatusOK
			wr.data = make([]byte, 0, 10)
			wr.hdrToSend = make(map[string][]string, 2)
			return wr
		},
	}

	p.log.Debug("velox middleware initialized",
		zap.String("server_url", p.cfg.ServerURL),
		zap.Duration("build_timeout", p.cfg.BuildTimeout),
		zap.Duration("request_timeout", p.cfg.RequestTimeout),
	)

	return nil
}

// Middleware implements the HTTP middleware interface
func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rrWriter := p.getWriter()
		defer func() {
			p.putWriter(rrWriter)
			_ = r.Body.Close()
		}()

		// Process request through next handler
		next.ServeHTTP(rrWriter, r)

		// Check for Velox build header
		if rrWriter.Header().Get(xVeloxBuildHeader) == "" {
			// Not a Velox build request, pass through
			p.passThrough(w, rrWriter)
			return
		}

		// Handle Velox build request
		p.handleVeloxBuild(w, r, rrWriter)
	})
}

// handleVeloxBuild processes Velox build requests
func (p *Plugin) handleVeloxBuild(w http.ResponseWriter, r *http.Request, rrWriter *writer) {
	ctx := r.Context()

	// Delete the Velox header from response
	rrWriter.Header().Del(xVeloxBuildHeader)

	// Decode response body if gzipped
	data := rrWriter.data
	if rrWriter.Header().Get("Content-Encoding") == "gzip" {
		p.log.Debug("response is gzip-encoded, decompressing")

		gzReader, err := gzip.NewReader(bytes.NewReader(rrWriter.data))
		if err != nil {
			p.log.Error("failed to create gzip reader",
				zap.Error(err),
			)
			http.Error(w, "Failed to decompress gzipped response", http.StatusBadRequest)
			return
		}
		defer gzReader.Close()

		decompressed, err := io.ReadAll(gzReader)
		if err != nil {
			p.log.Error("failed to decompress gzip data",
				zap.Error(err),
			)
			http.Error(w, "Failed to decompress gzipped response", http.StatusBadRequest)
			return
		}

		data = decompressed
		p.log.Debug("gzip decompression successful",
			zap.Int("compressed_size", len(rrWriter.data)),
			zap.Int("decompressed_size", len(data)),
		)
	}

	// Parse build request from response body
	var buildReq BuildRequest
	if err := json.Unmarshal(data, &buildReq); err != nil {
		p.log.Error("failed to parse build request",
			zap.Error(err),
			zap.String("body", string(data)),
		)
		http.Error(w, "Invalid build request format", http.StatusBadRequest)
		return
	}

	p.log.Debug("processing velox build request",
		zap.String("request_id", buildReq.RequestID),
		zap.String("os", buildReq.TargetPlatform.OS),
		zap.String("arch", buildReq.TargetPlatform.Arch),
		zap.String("rr_version", buildReq.RRVersion),
		zap.Int("plugins_count", len(buildReq.Plugins)),
	)

	// Build context with timeout
	buildCtx, cancel := context.WithTimeout(ctx, p.cfg.BuildTimeout)
	defer cancel()

	// Send build request to Velox server
	veloxResp, err := p.requestBuild(buildCtx, &buildReq)
	if err != nil {
		p.log.Error("velox build request failed",
			zap.Error(err),
			zap.String("request_id", buildReq.RequestID),
		)

		if errors.Is(err, context.DeadlineExceeded) {
			http.Error(w, "Build timeout exceeded", http.StatusGatewayTimeout)
		} else {
			http.Error(w, fmt.Sprintf("Build request failed: %v", err), http.StatusBadGateway)
		}
		return
	}

	// Validate file path
	if veloxResp.Path == "" {
		p.log.Error("velox response missing file path",
			zap.String("request_id", buildReq.RequestID),
		)
		http.Error(w, fmt.Sprintf("Build failed: %s", veloxResp.Logs), http.StatusInternalServerError)
		return
	}

	p.log.Debug("velox build completed",
		zap.String("request_id", buildReq.RequestID),
		zap.String("file_path", veloxResp.Path),
	)

	// Stream file to client
	if err := p.streamFile(w, veloxResp.Path); err != nil {
		p.log.Error("failed to stream file",
			zap.Error(err),
			zap.String("request_id", buildReq.RequestID),
			zap.String("file_path", veloxResp.Path),
		)
		// Can't write error response after starting file stream
		return
	}

	p.log.Debug("file streamed successfully",
		zap.String("request_id", buildReq.RequestID),
		zap.String("file_path", veloxResp.Path),
	)
}

// requestBuild sends a build request to Velox server with retry logic
func (p *Plugin) requestBuild(ctx context.Context, buildReq *BuildRequest) (*VeloxResponse, error) {
	const op = rrerrors.Op("velox_request_build")

	// Marshal build request
	payload, err := json.Marshal(buildReq)
	if err != nil {
		return nil, rrerrors.E(op, err)
	}

	var lastErr error
	delay := p.cfg.Retry.InitialDelay

	for attempt := 0; attempt < p.cfg.Retry.MaxAttempts; attempt++ {
		if attempt > 0 {
			// Wait before retry
			select {
			case <-ctx.Done():
				return nil, rrerrors.E(op, ctx.Err())
			case <-time.After(delay):
			}

			p.log.Warn("retrying velox build request",
				zap.Int("attempt", attempt+1),
				zap.Int("max_attempts", p.cfg.Retry.MaxAttempts),
				zap.Duration("delay", delay),
			)

			// Calculate next delay with exponential backoff
			delay = time.Duration(float64(delay) * p.cfg.Retry.BackoffMultiplier)
			if delay > p.cfg.Retry.MaxDelay {
				delay = p.cfg.Retry.MaxDelay
			}
		}

		// Create HTTP request
		buildURL := strings.TrimRight(p.cfg.ServerURL, "/") + buildEndpoint
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, buildURL, bytes.NewReader(payload))
		if err != nil {
			lastErr = err
			continue
		}

		req.Header.Set("Content-Type", ContentTypeJSON)

		// Send request
		resp, err := p.httpClient.Do(req)
		if err != nil {
			lastErr = err
			continue
		}

		// Read response body
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()

		if err != nil {
			lastErr = err
			continue
		}

		// Check HTTP status
		if resp.StatusCode != http.StatusOK {
			lastErr = fmt.Errorf("velox server returned status %d: %s", resp.StatusCode, string(body))
			continue
		}

		// Parse response
		var veloxResp VeloxResponse
		if err := json.Unmarshal(body, &veloxResp); err != nil {
			lastErr = fmt.Errorf("failed to parse velox response: %w", err)
			continue
		}

		// Success
		return &veloxResp, nil
	}

	return nil, rrerrors.E(op, fmt.Errorf("all retry attempts failed: %w", lastErr))
}

// streamFile streams a file to the HTTP response writer
func (p *Plugin) streamFile(w http.ResponseWriter, filePath string) error {
	const op = rrerrors.Op("velox_stream_file")

	// Security check: prevent path traversal
	if strings.Contains(filePath, "..") {
		return rrerrors.E(op, fmt.Errorf("invalid file path: contains '..'"))
	}

	// Check if file exists
	fs, err := os.Stat(filePath)
	if err != nil {
		return rrerrors.E(op, err)
	}

	// Open file for reading
	f, err := os.OpenFile(filePath, os.O_RDONLY, 0)
	if err != nil {
		return rrerrors.E(op, err)
	}
	defer func() {
		_ = f.Close()
	}()

	// Set response headers
	w.Header().Set("Content-Type", ContentTypeOctetStream)
	w.Header().Set("Content-Length", fmt.Sprintf("%d", fs.Size()))
	w.WriteHeader(http.StatusOK)

	size := fs.Size()
	var buf []byte

	// Allocate buffer based on file size
	if size < int64(bufSize) {
		// Small file: allocate exact size
		buf = make([]byte, size)
	} else {
		// Large file: use fixed 10MB buffer
		buf = make([]byte, bufSize)
	}

	off := 0
	for {
		n, err := f.ReadAt(buf, int64(off))
		if err != nil {
			if errors.Is(err, io.EOF) {
				if n > 0 {
					goto write
				}
				break
			}
			return rrerrors.E(op, err)
		}

	write:
		buf = buf[:n]
		_, err = w.Write(buf)
		if err != nil {
			return rrerrors.E(op, err)
		}

		// Flush data to client
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}

		off += n
	}

	return nil
}

// passThrough writes the original response from PHP worker to the client
func (p *Plugin) passThrough(w http.ResponseWriter, rrWriter *writer) {
	// Copy all headers
	for k := range rrWriter.hdrToSend {
		for _, v := range rrWriter.hdrToSend[k] {
			w.Header().Add(k, v)
		}
	}

	// Write status code
	w.WriteHeader(rrWriter.code)

	// Write body if exists
	if len(rrWriter.data) > 0 {
		_, err := w.Write(rrWriter.data)
		if err != nil {
			p.log.Error("failed to write response data", zap.Error(err))
		}
	}
}

// Name returns the plugin name
func (p *Plugin) Name() string {
	return PluginName
}

// getWriter retrieves a writer from the pool
func (p *Plugin) getWriter() *writer {
	return p.writersPool.Get().(*writer)
}

// putWriter returns a writer to the pool
func (p *Plugin) putWriter(w *writer) {
	w.code = http.StatusOK
	w.data = make([]byte, 0, 10)

	for k := range w.hdrToSend {
		delete(w.hdrToSend, k)
	}

	p.writersPool.Put(w)
}
