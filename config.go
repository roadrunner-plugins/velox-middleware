package veloxmiddleware

import (
	"fmt"
	"time"

	"github.com/roadrunner-server/errors"
)

// Config represents the Velox middleware plugin configuration.
type Config struct {
	// ServerURL is the Velox server endpoint (required)
	ServerURL string `mapstructure:"server_url"`

	// BuildTimeout is the maximum duration for a build operation
	BuildTimeout time.Duration `mapstructure:"build_timeout"`

	// MaxConcurrentBuilds limits the number of concurrent builds
	MaxConcurrentBuilds int `mapstructure:"max_concurrent_builds"`

	// RequestTimeout is the timeout for Velox server HTTP requests
	RequestTimeout time.Duration `mapstructure:"request_timeout"`

	// Cache configuration
	Cache CacheConfig `mapstructure:"cache"`

	// Retry configuration
	Retry RetryConfig `mapstructure:"retry"`
}

// CacheConfig represents cache-related configuration.
type CacheConfig struct {
	// Enabled enables or disables caching
	Enabled bool `mapstructure:"enabled"`

	// Dir is the cache directory path
	Dir string `mapstructure:"dir"`

	// TTLDays is the cache entry time-to-live in days
	TTLDays int `mapstructure:"ttl_days"`

	// MaxSizeMB is the maximum cache size in megabytes
	MaxSizeMB int64 `mapstructure:"max_size_mb"`

	// CleanupInterval is how often to run cache cleanup
	CleanupInterval time.Duration `mapstructure:"cleanup_interval"`
}

// RetryConfig represents retry-related configuration.
type RetryConfig struct {
	// MaxAttempts is the maximum number of retry attempts
	MaxAttempts int `mapstructure:"max_attempts"`

	// InitialDelay is the initial delay before first retry
	InitialDelay time.Duration `mapstructure:"initial_delay"`

	// MaxDelay is the maximum delay between retries
	MaxDelay time.Duration `mapstructure:"max_delay"`
}

// Validate validates the configuration and sets defaults.
func (c *Config) Validate() error {
	const op = errors.Op("velox_middleware_config_validate")

	// Validate ServerURL (required)
	if c.ServerURL == "" {
		return errors.E(op, errors.Str("server_url is required"))
	}

	// Set defaults for build timeout
	if c.BuildTimeout == 0 {
		c.BuildTimeout = 5 * time.Minute
	}

	// Set defaults for max concurrent builds
	if c.MaxConcurrentBuilds == 0 {
		c.MaxConcurrentBuilds = 5
	}
	if c.MaxConcurrentBuilds < 1 {
		return errors.E(op, errors.Str("max_concurrent_builds must be at least 1"))
	}

	// Set defaults for request timeout
	if c.RequestTimeout == 0 {
		c.RequestTimeout = 10 * time.Second
	}

	// Validate cache configuration
	if err := c.Cache.validate(); err != nil {
		return errors.E(op, err)
	}

	// Validate retry configuration
	if err := c.Retry.validate(); err != nil {
		return errors.E(op, err)
	}

	return nil
}

// validate validates cache configuration and sets defaults.
func (cc *CacheConfig) validate() error {
	const op = errors.Op("cache_config_validate")

	// Cache is enabled by default
	if !cc.Enabled {
		// No further validation needed if cache is disabled
		return nil
	}

	// Set default cache directory
	if cc.Dir == "" {
		cc.Dir = "./.rr-cache/velox-binaries"
	}

	// Set default TTL
	if cc.TTLDays == 0 {
		cc.TTLDays = 7
	}
	if cc.TTLDays < 0 {
		return errors.E(op, errors.Str("ttl_days must be positive"))
	}

	// Set default max size
	if cc.MaxSizeMB == 0 {
		cc.MaxSizeMB = 5000 // 5GB
	}
	if cc.MaxSizeMB < 100 {
		return errors.E(op, errors.Str("max_size_mb must be at least 100MB"))
	}

	// Set default cleanup interval
	if cc.CleanupInterval == 0 {
		cc.CleanupInterval = 1 * time.Hour
	}
	if cc.CleanupInterval < 1*time.Minute {
		return errors.E(op, errors.Str("cleanup_interval must be at least 1 minute"))
	}

	return nil
}

// validate validates retry configuration and sets defaults.
func (rc *RetryConfig) validate() error {
	const op = errors.Op("retry_config_validate")

	// Set default max attempts
	if rc.MaxAttempts == 0 {
		rc.MaxAttempts = 3
	}
	if rc.MaxAttempts < 1 {
		return errors.E(op, errors.Str("max_attempts must be at least 1"))
	}

	// Set default initial delay
	if rc.InitialDelay == 0 {
		rc.InitialDelay = 1 * time.Second
	}

	// Set default max delay
	if rc.MaxDelay == 0 {
		rc.MaxDelay = 10 * time.Second
	}
	if rc.MaxDelay < rc.InitialDelay {
		return errors.E(op, fmt.Errorf("max_delay (%v) must be >= initial_delay (%v)", rc.MaxDelay, rc.InitialDelay))
	}

	return nil
}

// TTL returns the cache TTL as a duration.
func (cc *CacheConfig) TTL() time.Duration {
	return time.Duration(cc.TTLDays) * 24 * time.Hour
}

// MaxSizeBytes returns the maximum cache size in bytes.
func (cc *CacheConfig) MaxSizeBytes() int64 {
	return cc.MaxSizeMB * 1024 * 1024
}
