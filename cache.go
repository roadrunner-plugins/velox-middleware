package velox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	rrerrors "github.com/roadrunner-server/errors"
	"go.uber.org/zap"
)

const (
	// metadataVersion is the current metadata format version
	metadataVersion = "1.0"

	// tempFilePrefix is used for temporary files during atomic writes
	tempFilePrefix = "temp_"

	// binaryFileSuffix is the extension for cached binary files
	binaryFileSuffix = ".bin"

	// metadataFileSuffix is the extension for metadata files
	metadataFileSuffix = ".json"
)

// CacheManager handles binary caching operations
type CacheManager struct {
	cfg      *CacheConfig
	log      *zap.Logger
	cacheDir string
	inFlight sync.Map // map[string]*sync.Mutex for in-flight builds
	updates  *updateBatch
	cleanup  *time.Ticker
	ctx      context.Context
	cancel   context.CancelFunc
	wg       sync.WaitGroup
}

// updateBatch tracks pending access updates
type updateBatch struct {
	mu      sync.Mutex
	pending map[string]accessUpdate
}

// accessUpdate represents a pending metadata update
type accessUpdate struct {
	timestamp int64
}

// NewCacheManager creates a new cache manager instance
func NewCacheManager(cfg *CacheConfig, log *zap.Logger) (*CacheManager, error) {
	const op = rrerrors.Op("cache_manager_new")

	if !cfg.Enabled {
		return nil, nil
	}

	// Ensure cache directory exists
	if err := os.MkdirAll(cfg.Directory, 0755); err != nil {
		return nil, rrerrors.E(op, fmt.Errorf("failed to create cache directory: %w", err))
	}

	ctx, cancel := context.WithCancel(context.Background())

	cm := &CacheManager{
		cfg:      cfg,
		log:      log,
		cacheDir: cfg.Directory,
		updates: &updateBatch{
			pending: make(map[string]accessUpdate),
		},
		ctx:    ctx,
		cancel: cancel,
	}

	// Start access update flusher if batching is enabled
	if cfg.AccessUpdateInterval > 0 {
		cm.wg.Add(1)
		go cm.flushAccessUpdates()
	}

	// Start periodic cleanup if enabled
	if cfg.CleanupInterval > 0 {
		cm.cleanup = time.NewTicker(cfg.CleanupInterval)
		cm.wg.Add(1)
		go cm.runPeriodicCleanup()
	}

	log.Info("cache manager initialized",
		zap.String("directory", cfg.Directory),
		zap.Float64("max_size_gb", cfg.MaxSizeGB),
		zap.Int("max_entries", cfg.MaxEntries),
		zap.Int("max_age_days", cfg.MaxAgeDays),
		zap.String("eviction_policy", cfg.EvictionPolicy),
	)

	return cm, nil
}

// Stop gracefully shuts down the cache manager
func (cm *CacheManager) Stop() error {
	if cm == nil {
		return nil
	}

	cm.log.Debug("stopping cache manager")

	// Cancel context to signal goroutines
	cm.cancel()

	// Stop cleanup ticker
	if cm.cleanup != nil {
		cm.cleanup.Stop()
	}

	// Wait for goroutines to finish
	cm.wg.Wait()

	// Flush any pending updates
	if err := cm.flushPendingUpdates(); err != nil {
		cm.log.Error("failed to flush pending updates on shutdown", zap.Error(err))
	}

	cm.log.Info("cache manager stopped")
	return nil
}

// Get retrieves a cached binary if it exists
func (cm *CacheManager) Get(buildReq *BuildRequest) (*CacheEntry, error) {
	const op = rrerrors.Op("cache_get")

	if cm == nil {
		return nil, nil
	}

	// Calculate hash
	hash := cm.calculateHash(buildReq)

	// Check if metadata file exists
	metadataPath := filepath.Join(cm.cacheDir, hash+metadataFileSuffix)
	if _, err := os.Stat(metadataPath); os.IsNotExist(err) {
		return nil, nil // Cache miss
	}

	// Load metadata
	entry, err := cm.loadMetadata(metadataPath)
	if err != nil {
		cm.log.Warn("failed to load metadata, treating as cache miss",
			zap.Error(err),
			zap.String("hash", hash),
		)
		// Try to clean up corrupted metadata
		_ = os.Remove(metadataPath)
		return nil, nil
	}

	// Verify binary file exists
	binaryPath := filepath.Join(cm.cacheDir, hash+binaryFileSuffix)
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		cm.log.Warn("binary file missing, removing orphaned metadata",
			zap.String("hash", hash),
		)
		_ = os.Remove(metadataPath)
		return nil, nil
	}

	// Update access tracking (async if batching enabled)
	if cm.cfg.AccessUpdateInterval > 0 {
		cm.trackAccess(hash)
	} else {
		// Immediate update
		entry.LastAccessed = time.Now().Unix()
		entry.AccessCount++
		if err := cm.saveMetadata(metadataPath, entry); err != nil {
			cm.log.Warn("failed to update access metadata", zap.Error(err))
			// Don't fail the cache hit due to metadata update failure
		}
	}

	cm.log.Debug("cache hit",
		zap.String("hash", hash),
		zap.String("rr_version", entry.RRVersion),
		zap.String("platform", fmt.Sprintf("%s/%s", entry.Platform.OS, entry.Platform.Arch)),
		zap.Int64("file_size", entry.FileSize),
	)

	return entry, nil
}

// Store saves a binary and its metadata to the cache
func (cm *CacheManager) Store(buildReq *BuildRequest, binaryPath string, buildDuration time.Duration) error {
	const op = rrerrors.Op("cache_store")

	if cm == nil {
		return nil
	}

	// Calculate hash
	hash := cm.calculateHash(buildReq)

	// Acquire build lock to prevent concurrent stores of same hash
	buildLock := cm.acquireBuildLock(hash)
	buildLock.Lock()
	defer func() {
		buildLock.Unlock()
		cm.releaseBuildLock(hash)
	}()

	// Get file size
	fileInfo, err := os.Stat(binaryPath)
	if err != nil {
		return rrerrors.E(op, fmt.Errorf("failed to stat binary file: %w", err))
	}

	targetBinaryPath := filepath.Join(cm.cacheDir, hash+binaryFileSuffix)
	targetMetadataPath := filepath.Join(cm.cacheDir, hash+metadataFileSuffix)

	// Copy binary file atomically
	if err := cm.copyFileAtomic(binaryPath, targetBinaryPath); err != nil {
		return rrerrors.E(op, fmt.Errorf("failed to copy binary: %w", err))
	}

	// Create metadata
	now := time.Now().Unix()
	entry := &CacheEntry{
		Version:         metadataVersion,
		Hash:            hash,
		BinaryFile:      hash + binaryFileSuffix,
		FileSize:        fileInfo.Size(),
		RRVersion:       buildReq.RRVersion,
		Platform:        buildReq.TargetPlatform,
		Plugins:         buildReq.Plugins,
		CreatedAt:       now,
		LastAccessed:    now,
		AccessCount:     1,
		BuildDurationMs: buildDuration.Milliseconds(),
	}

	// Save metadata atomically
	if err := cm.saveMetadata(targetMetadataPath, entry); err != nil {
		// Clean up binary on metadata save failure
		_ = os.Remove(targetBinaryPath)
		return rrerrors.E(op, fmt.Errorf("failed to save metadata: %w", err))
	}

	cm.log.Info("binary cached successfully",
		zap.String("hash", hash),
		zap.String("rr_version", entry.RRVersion),
		zap.String("platform", fmt.Sprintf("%s/%s", entry.Platform.OS, entry.Platform.Arch)),
		zap.Int64("file_size", entry.FileSize),
		zap.Int64("build_duration_ms", entry.BuildDurationMs),
	)

	return nil
}

// GetBinaryPath returns the full path to a cached binary
func (cm *CacheManager) GetBinaryPath(hash string) string {
	return filepath.Join(cm.cacheDir, hash+binaryFileSuffix)
}

// calculateHash generates a deterministic hash from build request
func (cm *CacheManager) calculateHash(req *BuildRequest) string {
	// Sort plugins for consistent hashing
	plugins := make([]BuildPlugin, len(req.Plugins))
	copy(plugins, req.Plugins)
	sort.Slice(plugins, func(i, j int) bool {
		if plugins[i].ModuleName != plugins[j].ModuleName {
			return plugins[i].ModuleName < plugins[j].ModuleName
		}
		return plugins[i].Tag < plugins[j].Tag
	})

	// Build canonical string
	var sb strings.Builder
	sb.WriteString(req.RRVersion)
	sb.WriteString("|")
	sb.WriteString(req.TargetPlatform.OS)
	sb.WriteString("|")
	sb.WriteString(req.TargetPlatform.Arch)
	for _, plugin := range plugins {
		sb.WriteString("|")
		sb.WriteString(plugin.ModuleName)
		sb.WriteString("@")
		sb.WriteString(plugin.Tag)
	}

	// Calculate SHA-256 hash
	hash := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(hash[:])
}

// loadMetadata loads and parses a metadata file
func (cm *CacheManager) loadMetadata(path string) (*CacheEntry, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read metadata file: %w", err)
	}

	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, fmt.Errorf("failed to parse metadata: %w", err)
	}

	return &entry, nil
}

// saveMetadata saves metadata to a file atomically
func (cm *CacheManager) saveMetadata(path string, entry *CacheEntry) error {
	// Marshal to JSON with indentation
	data, err := json.MarshalIndent(entry, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal metadata: %w", err)
	}

	// Write atomically using temp file + rename
	tempPath := filepath.Join(cm.cacheDir, tempFilePrefix+uuid.New().String()+metadataFileSuffix)

	if err := os.WriteFile(tempPath, data, 0644); err != nil {
		return fmt.Errorf("failed to write temp metadata: %w", err)
	}

	// Sync to disk
	if f, err := os.OpenFile(tempPath, os.O_RDWR, 0644); err == nil {
		_ = f.Sync()
		_ = f.Close()
	}

	// Atomic rename
	if err := os.Rename(tempPath, path); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to rename metadata file: %w", err)
	}

	return nil
}

// copyFileAtomic copies a file atomically using temp file + rename
func (cm *CacheManager) copyFileAtomic(src, dst string) error {
	// Open source file
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("failed to open source file: %w", err)
	}
	defer srcFile.Close()

	// Create temp file in cache directory
	tempPath := filepath.Join(cm.cacheDir, tempFilePrefix+uuid.New().String()+binaryFileSuffix)
	tempFile, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return fmt.Errorf("failed to create temp file: %w", err)
	}

	// Copy data
	_, err = io.Copy(tempFile, srcFile)
	if err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to copy data: %w", err)
	}

	// Sync to disk
	if err := tempFile.Sync(); err != nil {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to sync temp file: %w", err)
	}

	_ = tempFile.Close()

	// Atomic rename
	if err := os.Rename(tempPath, dst); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("failed to rename file: %w", err)
	}

	return nil
}

// acquireBuildLock gets or creates a lock for a specific hash
func (cm *CacheManager) acquireBuildLock(hash string) *sync.Mutex {
	mu, _ := cm.inFlight.LoadOrStore(hash, &sync.Mutex{})
	return mu.(*sync.Mutex)
}

// releaseBuildLock removes the lock for a specific hash
func (cm *CacheManager) releaseBuildLock(hash string) {
	cm.inFlight.Delete(hash)
}

// trackAccess records an access event for batched updates
func (cm *CacheManager) trackAccess(hash string) {
	cm.updates.mu.Lock()
	cm.updates.pending[hash] = accessUpdate{
		timestamp: time.Now().Unix(),
	}
	cm.updates.mu.Unlock()
}

// flushAccessUpdates periodically flushes pending access updates
func (cm *CacheManager) flushAccessUpdates() {
	defer cm.wg.Done()

	ticker := time.NewTicker(cm.cfg.AccessUpdateInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			if err := cm.flushPendingUpdates(); err != nil {
				cm.log.Error("failed to flush access updates", zap.Error(err))
			}
		case <-cm.ctx.Done():
			return
		}
	}
}

// flushPendingUpdates writes all pending access updates to disk
func (cm *CacheManager) flushPendingUpdates() error {
	cm.updates.mu.Lock()
	pending := cm.updates.pending
	cm.updates.pending = make(map[string]accessUpdate)
	cm.updates.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}

	cm.log.Debug("flushing access updates", zap.Int("count", len(pending)))

	for hash, update := range pending {
		metadataPath := filepath.Join(cm.cacheDir, hash+metadataFileSuffix)

		entry, err := cm.loadMetadata(metadataPath)
		if err != nil {
			cm.log.Warn("failed to load metadata for access update",
				zap.Error(err),
				zap.String("hash", hash),
			)
			continue
		}

		entry.LastAccessed = update.timestamp
		entry.AccessCount++

		if err := cm.saveMetadata(metadataPath, entry); err != nil {
			cm.log.Warn("failed to save updated metadata",
				zap.Error(err),
				zap.String("hash", hash),
			)
		}
	}

	return nil
}

// runPeriodicCleanup runs cache cleanup at configured intervals
func (cm *CacheManager) runPeriodicCleanup() {
	defer cm.wg.Done()

	for {
		select {
		case <-cm.cleanup.C:
			if err := cm.Cleanup(); err != nil {
				cm.log.Error("cache cleanup failed", zap.Error(err))
			}
		case <-cm.ctx.Done():
			return
		}
	}
}
