package veloxmiddleware

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/roadrunner-server/errors"
	"go.uber.org/zap"
)

const (
	// PluginName is the unique identifier for the Velox middleware plugin
	PluginName = "velox"

	// HeaderVeloxBuild is the HTTP header that triggers Velox build interception
	HeaderVeloxBuild = "X-Velox-Build"
)

// Plugin represents the Velox middleware plugin that intercepts HTTP responses
// containing Velox build requests, manages caching, and streams binaries directly
// to clients without blocking PHP workers.
type Plugin struct {
	cfg    *Config
	log    *zap.Logger
	cache  *CacheManager
	client *VeloxClient
	sem    *semaphore // limits concurrent builds

	// Response writer pool for capturing PHP worker responses
	writersPool sync.Pool

	// Lifecycle management
	mu      sync.RWMutex
	serving bool
	stopCh  chan struct{}
}

// Init initializes the plugin with configuration and dependencies.
// This method is called by the Endure container during plugin initialization.
func (p *Plugin) Init(cfg Configurer, log Logger) error {
	const op = errors.Op("velox_plugin_init")

	if !cfg.Has(PluginName) {
		return errors.E(op, errors.Disabled)
	}

	// Parse configuration
	if err := cfg.UnmarshalKey(PluginName, &p.cfg); err != nil {
		return errors.E(op, err)
	}

	// Validate configuration
	if err := p.cfg.Validate(); err != nil {
		return errors.E(op, err)
	}

	p.log = log.NamedLogger(PluginName)
	p.stopCh = make(chan struct{})

	// Initialize response writer pool for capturing PHP responses
	p.writersPool = sync.Pool{
		New: func() any {
			wr := new(responseWriter)
			wr.code = http.StatusOK
			wr.data = make([]byte, 0, 1024) // Initial capacity for response body
			wr.hdrToSend = make(map[string][]string, 10)
			return wr
		},
	}

	// Initialize metrics early
	initMetrics()

	// Initialize semaphore for build concurrency control
	p.sem = newSemaphore(p.cfg.MaxConcurrentBuilds)

	// Initialize Velox client
	client, err := NewVeloxClient(p.cfg, p.log)
	if err != nil {
		return errors.E(op, errors.Str("failed to create velox client"), err)
	}
	p.client = client

	// Initialize cache manager
	if p.cfg.Cache.Enabled {
		cache, err := NewCacheManager(p.cfg.Cache, p.log)
		if err != nil {
			return errors.E(op, errors.Str("failed to initialize cache"), err)
		}
		p.cache = cache
	} else {
		p.log.Info("cache disabled, all builds will go directly to Velox server")
	}

	p.log.Info("plugin initialized",
		zap.String("server_url", p.cfg.ServerURL),
		zap.Bool("cache_enabled", p.cfg.Cache.Enabled),
		zap.Int("max_concurrent_builds", p.cfg.MaxConcurrentBuilds),
		zap.Duration("build_timeout", p.cfg.BuildTimeout),
	)

	return nil
}

// Serve starts the plugin's background services.
// This includes cache cleanup goroutines and index persistence.
func (p *Plugin) Serve() chan error {
	errCh := make(chan error, 1)

	p.mu.Lock()
	p.serving = true
	p.mu.Unlock()

	// Start cache background tasks if enabled
	if p.cache != nil {
		go p.cache.StartBackgroundTasks(p.stopCh)
	}

	p.log.Info("plugin serving")

	return errCh
}

// Stop gracefully shuts down the plugin.
// This waits for active builds to complete and persists cache state.
func (p *Plugin) Stop(ctx context.Context) error {
	p.mu.Lock()
	if !p.serving {
		p.mu.Unlock()
		return nil
	}
	p.serving = false
	p.mu.Unlock()

	close(p.stopCh)

	// Wait for active builds to complete or context timeout
	done := make(chan struct{})
	go func() {
		// Wait for semaphore to drain (all builds complete)
		for p.sem.active() > 0 {
			time.Sleep(100 * time.Millisecond)
		}
		close(done)
	}()

	select {
	case <-done:
		p.log.Info("all active builds completed")
	case <-ctx.Done():
		p.log.Warn("shutdown timeout reached, some builds may have been interrupted",
			zap.Int("active_builds", p.sem.active()),
		)
	}

	// Shutdown cache manager
	if p.cache != nil {
		if err := p.cache.Stop(ctx); err != nil {
			p.log.Error("failed to stop cache manager", zap.Error(err))
			return err
		}
	}

	// Close Velox client
	if err := p.client.Close(); err != nil {
		p.log.Error("failed to close velox client", zap.Error(err))
		return err
	}

	p.log.Info("plugin stopped")
	return nil
}

// Name returns the plugin name for registration in the Endure container.
func (p *Plugin) Name() string {
	return PluginName
}

// Weight returns the plugin weight for dependency resolution.
// Higher weight means the plugin will be initialized/served later.
// Velox needs to be initialized before HTTP plugin (weight 10).
func (p *Plugin) Weight() uint {
	return 9
}

// Middleware returns the HTTP middleware function that will be registered with RoadRunner's HTTP plugin.
// This function intercepts responses from PHP workers and handles Velox build requests when detected.
//
// The middleware wraps the response writer to capture headers and body from PHP workers.
// If a Velox build request is detected in the response, it intercepts and handles the build,
// otherwise it passes the original response through to the client.
//
// The HTTP plugin will automatically discover and register this middleware if the plugin:
// 1. Implements the Middleware(next http.Handler) http.Handler method
// 2. Implements the Name() string method
// 3. Is properly initialized in the Endure container
func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Create a response writer wrapper to capture PHP worker response
		rrWriter := p.getWriter()
		defer p.putWriter(rrWriter)

		// Pass request to PHP worker
		next.ServeHTTP(rrWriter, r)

		// Check if PHP worker returned a Velox build request
		if rrWriter.Header().Get(HeaderVeloxBuild) != "true" {
			// Not a Velox request, pass through original response
			p.passthroughResponse(w, rrWriter)
			return
		}

		// Velox build request detected - intercept and handle
		p.log.Info("velox build request detected from PHP worker",
			zap.String("path", r.URL.Path),
			zap.String("method", r.Method),
		)

		// Handle Velox build request using the response body from PHP
		p.handleVeloxBuild(w, r, rrWriter.data)
	})
}

// getWriter retrieves a response writer from the pool.
func (p *Plugin) getWriter() *responseWriter {
	return p.writersPool.Get().(*responseWriter)
}

// putWriter returns a response writer to the pool after resetting its state.
func (p *Plugin) putWriter(w *responseWriter) {
	w.code = http.StatusOK
	w.data = w.data[:0] // Reset slice but keep capacity

	// Clear headers map
	for k := range w.hdrToSend {
		delete(w.hdrToSend, k)
	}

	p.writersPool.Put(w)
}

// passthroughResponse writes the original PHP worker response to the client.
// This is called when the response doesn't contain a Velox build request.
func (p *Plugin) passthroughResponse(w http.ResponseWriter, rrWriter *responseWriter) {
	// Copy all headers from PHP worker response
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
			p.log.Error("failed to write passthrough response", zap.Error(err))
		}
	}
}
