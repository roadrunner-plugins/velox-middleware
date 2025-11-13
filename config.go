package velox

import (
	"fmt"
	"time"

	rrerrors "github.com/roadrunner-server/errors"
)

// Config holds the plugin configuration
type Config struct {
	// ServerURL is the Velox build server base URL (required)
	// The build endpoint path will be appended automatically
	ServerURL string `mapstructure:"server_url"`

	// BuildTimeout is the maximum time to wait for build completion (default: 5m)
	BuildTimeout time.Duration `mapstructure:"build_timeout"`

	// RequestTimeout is the HTTP request timeout for Velox communication (default: 180s)
	RequestTimeout time.Duration `mapstructure:"request_timeout"`

	// Retry configuration for Velox server communication
	Retry RetryConfig `mapstructure:"retry"`

	// Cache configuration for binary caching
	Cache CacheConfig `mapstructure:"cache"`
}

// RetryConfig holds retry configuration
type RetryConfig struct {
	// MaxAttempts is the maximum number of retry attempts (default: 3)
	MaxAttempts int `mapstructure:"max_attempts"`

	// InitialDelay is the initial delay before first retry (default: 1s)
	InitialDelay time.Duration `mapstructure:"initial_delay"`

	// MaxDelay is the maximum delay between retries (default: 10s)
	MaxDelay time.Duration `mapstructure:"max_delay"`

	// BackoffMultiplier is the backoff multiplier for exponential backoff (default: 2.0)
	BackoffMultiplier float64 `mapstructure:"backoff_multiplier"`
}

// InitDefaults sets default values for the configuration
func (c *Config) InitDefaults() {
	if c.BuildTimeout == 0 {
		c.BuildTimeout = 5 * time.Minute
	}

	if c.RequestTimeout == 0 {
		c.RequestTimeout = 180 * time.Second
	}

	if c.Retry.MaxAttempts == 0 {
		c.Retry.MaxAttempts = 3
	}

	if c.Retry.InitialDelay == 0 {
		c.Retry.InitialDelay = 1 * time.Second
	}

	if c.Retry.MaxDelay == 0 {
		c.Retry.MaxDelay = 10 * time.Second
	}

	if c.Retry.BackoffMultiplier == 0 {
		c.Retry.BackoffMultiplier = 2.0
	}

	// Initialize cache defaults
	c.Cache.InitDefaults()
}

// Validate validates the configuration
func (c *Config) Validate() error {
	const op = rrerrors.Op("velox_config_validate")

	if c.ServerURL == "" {
		return rrerrors.E(op, fmt.Errorf("server_url is required"))
	}

	if c.BuildTimeout < time.Second {
		return rrerrors.E(op, fmt.Errorf("build_timeout must be at least 1 second"))
	}

	if c.RequestTimeout < time.Second {
		return rrerrors.E(op, fmt.Errorf("request_timeout must be at least 1 second"))
	}

	if c.Retry.MaxAttempts < 1 {
		return rrerrors.E(op, fmt.Errorf("retry.max_attempts must be at least 1"))
	}

	if c.Retry.InitialDelay < 0 {
		return rrerrors.E(op, fmt.Errorf("retry.initial_delay cannot be negative"))
	}

	if c.Retry.MaxDelay < c.Retry.InitialDelay {
		return rrerrors.E(op, fmt.Errorf("retry.max_delay must be greater than or equal to initial_delay"))
	}

	if c.Retry.BackoffMultiplier < 1.0 {
		return rrerrors.E(op, fmt.Errorf("retry.backoff_multiplier must be at least 1.0"))
	}

	return nil
}

// BuildRequest represents the build request from PHP
type BuildRequest struct {
	// RequestID is a unique identifier for the build request
	RequestID string `json:"request_id"`

	// TargetPlatform specifies the target OS and architecture
	TargetPlatform Platform `json:"target_platform"`

	// RRVersion is the RoadRunner version to build
	RRVersion string `json:"rr_version"`

	// Plugins is the list of plugins to include in the build
	Plugins []BuildPlugin `json:"plugins"`
}

// Platform represents the target platform
type Platform struct {
	// OS is the target operating system (windows, linux, darwin)
	OS string `json:"os"`

	// Arch is the target architecture (amd64, arm64)
	Arch string `json:"arch"`
}

// BuildPlugin represents a RoadRunner plugin in the build request
type BuildPlugin struct {
	// ModuleName is the full Go module path
	ModuleName string `json:"module_name"`

	// Tag is the Git tag or version
	Tag string `json:"tag"`
}

// VeloxResponse represents the response from Velox server
type VeloxResponse struct {
	Path string `json:"path"`
	Logs string `json:"logs"`
}
