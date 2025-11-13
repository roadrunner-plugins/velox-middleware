package velox

import "time"

// CacheConfig holds cache configuration
type CacheConfig struct {
	// Enabled enables or disables caching
	Enabled bool `mapstructure:"enabled"`

	// Directory is the cache storage location
	Directory string `mapstructure:"directory"`

	// MaxSizeGB is the maximum total cache size in gigabytes (0 = unlimited)
	MaxSizeGB float64 `mapstructure:"max_size_gb"`

	// MaxEntries is the maximum number of cached binaries (0 = unlimited)
	MaxEntries int `mapstructure:"max_entries"`

	// MaxAgeDays is the maximum age for cache entries in days (0 = never expire)
	MaxAgeDays int `mapstructure:"max_age_days"`

	// CleanupInterval is how often to run cleanup (0 = disabled)
	CleanupInterval time.Duration `mapstructure:"cleanup_interval"`

	// EvictionPolicy determines which entries to remove when limits are exceeded
	// Supported: "lru" (least recently used), "lfu" (least frequently used), "fifo" (first in first out)
	EvictionPolicy string `mapstructure:"eviction_policy"`

	// AccessUpdateInterval is how often to update access metadata (0 = immediate)
	AccessUpdateInterval time.Duration `mapstructure:"access_update_interval"`
}

// InitDefaults sets default values for cache configuration
func (c *CacheConfig) InitDefaults() {
	if c.Directory == "" {
		c.Directory = "./cache/velox"
	}

	if c.CleanupInterval == 0 {
		c.CleanupInterval = 1 * time.Hour
	}

	if c.EvictionPolicy == "" {
		c.EvictionPolicy = "lru"
	}

	if c.AccessUpdateInterval == 0 {
		c.AccessUpdateInterval = 5 * time.Minute
	}
}

// CacheEntry represents metadata for a cached binary
type CacheEntry struct {
	// Version is the metadata format version
	Version string `json:"version"`

	// Hash is the unique identifier for this cache entry
	Hash string `json:"hash"`

	// BinaryFile is the filename of the cached binary
	BinaryFile string `json:"binary_file"`

	// FileSize is the size of the binary file in bytes
	FileSize int64 `json:"file_size"`

	// RRVersion is the RoadRunner version
	RRVersion string `json:"rr_version"`

	// Platform is the target platform
	Platform Platform `json:"platform"`

	// Plugins is the list of plugins in this build
	Plugins []BuildPlugin `json:"plugins"`

	// CreatedAt is when this entry was created (Unix timestamp)
	CreatedAt int64 `json:"created_at"`

	// LastAccessed is when this entry was last accessed (Unix timestamp)
	LastAccessed int64 `json:"last_accessed"`

	// AccessCount is how many times this entry has been accessed
	AccessCount int `json:"access_count"`

	// BuildDurationMs is the original build duration in milliseconds
	BuildDurationMs int64 `json:"build_duration_ms"`
}
