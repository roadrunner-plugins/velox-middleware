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
	PluginName = "velox-middleware"

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

	// Lifecycle management
	mu      sync.RWMutex
	serving bool
	stopCh  chan struct{}
}

// Init initializes the plugin with configuration and dependencies.
// This method is called by the Endure container during plugin initialization.
func (p *Plugin) Init(cfg Configurer, log Logger) error {
	const op = errors.Op("velox_middleware_plugin_init")

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

// Middleware returns the HTTP middleware function that will be registered with RoadRunner's HTTP plugin.
// This function intercepts requests and handles Velox build requests when the special header is present.
func (p *Plugin) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Check if this is a Velox build request
		if r.Header.Get(HeaderVeloxBuild) != "true" {
			// Not a Velox request, pass through to next handler
			next.ServeHTTP(w, r)
			return
		}

		// Handle Velox build request
		p.handleVeloxBuild(w, r)
	})
}

// Name returns the plugin name for registration in the Endure container.
func (p *Plugin) Name() string {
	return PluginName
}

// Weight returns the plugin weight for dependency resolution.
// Higher weight means the plugin will be initialized/served later.
func (p *Plugin) Weight() uint {
	return 10
}

// RPC returns nil as this plugin doesn't expose RPC methods.
func (p *Plugin) RPC() interface{} {
	return nil
}
