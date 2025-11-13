package veloxmiddleware

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"go.uber.org/zap"
)

// CacheEntry represents a cached binary with metadata.
type CacheEntry struct {
	Key        string    `json:"key"`         // SHA256 hash (first 16 chars)
	FilePath   string    `json:"file_path"`   // Relative path to cached binary
	FileSize   int64     `json:"file_size"`   // Binary size in bytes
	CreatedAt  time.Time `json:"created_at"`  // When binary was cached
	ExpiresAt  time.Time `json:"expires_at"`  // When entry expires
	AccessedAt time.Time `json:"accessed_at"` // Last access time (for LRU)

	BuildRequest BuildRequest `json:"build_config"` // Original build configuration
}

// CacheIndex represents the in-memory cache index with LRU tracking.
type CacheIndex struct {
	mu      sync.RWMutex
	entries map[string]*CacheEntry // key: hash -> entry

	// LRU tracking
	lruList *list.List               // doubly-linked list for LRU
	lruMap  map[string]*list.Element // hash -> list element
}

// CacheManager manages binary cache operations.
type CacheManager struct {
	cfg       CacheConfig
	log       *zap.Logger
	index     *CacheIndex
	indexPath string // Path to index.json file

	// Background task control
	persistTicker *time.Ticker
	cleanupTicker *time.Ticker
	mu            sync.RWMutex
}

// NewCacheManager creates a new cache manager.
func NewCacheManager(cfg CacheConfig, log *zap.Logger) (*CacheManager, error) {
	cm := &CacheManager{
		cfg: cfg,
		log: log.Named("cache"),
		index: &CacheIndex{
			entries: make(map[string]*CacheEntry),
			lruList: list.New(),
			lruMap:  make(map[string]*list.Element),
		},
		indexPath: filepath.Join(cfg.Dir, "index.json"),
	}

	// Create cache directory
	if err := os.MkdirAll(cfg.Dir, 0755); err != nil {
		return nil, fmt.Errorf("failed to create cache directory: %w", err)
	}

	// Load existing cache index
	if err := cm.loadIndex(); err != nil {
		cm.log.Warn("failed to load cache index, starting fresh",
			zap.Error(err),
		)
	}

	cm.log.Info("cache manager initialized",
		zap.String("dir", cfg.Dir),
		zap.Int("entries_loaded", len(cm.index.entries)),
		zap.Duration("ttl", cfg.TTL()),
		zap.Int64("max_size_mb", cfg.MaxSizeMB),
	)

	return cm, nil
}

// Get retrieves a cache entry by key.
func (cm *CacheManager) Get(key string) (*CacheEntry, bool) {
	cm.index.mu.RLock()
	defer cm.index.mu.RUnlock()

	entry, found := cm.index.entries[key]
	if !found {
		return nil, false
	}

	// Check if expired
	if time.Now().After(entry.ExpiresAt) {
		return nil, false
	}

	return entry, true
}

// UpdateAccessTime updates the access time for an entry (for LRU).
func (cm *CacheManager) UpdateAccessTime(key string) {
	cm.index.mu.Lock()
	defer cm.index.mu.Unlock()

	entry, found := cm.index.entries[key]
	if !found {
		return
	}

	entry.AccessedAt = time.Now()

	// Move to front of LRU list
	if elem, ok := cm.index.lruMap[key]; ok {
		cm.index.lruList.MoveToFront(elem)
	}

	metricsSetCacheAccessAge(time.Since(entry.CreatedAt))
}

// PrepareWrite prepares a temporary file for writing a new cache entry.
func (cm *CacheManager) PrepareWrite(key string) (io.WriteCloser, string, error) {
	// Create hierarchical directory structure
	dir := cm.getCacheFilePath(key)
	if err := os.MkdirAll(filepath.Dir(dir), 0755); err != nil {
		return nil, "", fmt.Errorf("failed to create cache subdirectory: %w", err)
	}

	// Create temp file
	tempPath := dir + ".tmp"
	file, err := os.Create(tempPath)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create temp file: %w", err)
	}

	return file, dir, nil
}

// CommitWrite commits a cache entry after successful write.
func (cm *CacheManager) CommitWrite(entry *CacheEntry) error {
	tempPath := entry.FilePath + ".tmp"

	// Atomic rename
	if err := os.Rename(tempPath, entry.FilePath); err != nil {
		return fmt.Errorf("failed to rename temp file: %w", err)
	}

	// Add to index
	cm.index.mu.Lock()
	defer cm.index.mu.Unlock()

	// Remove old entry if exists (force rebuild case)
	if oldEntry, found := cm.index.entries[entry.Key]; found {
		cm.removeEntryLocked(oldEntry)
	}

	cm.index.entries[entry.Key] = entry

	// Add to LRU list
	elem := cm.index.lruList.PushFront(entry.Key)
	cm.index.lruMap[entry.Key] = elem

	// Update metrics
	metricsSetCacheEntries(len(cm.index.entries))
	metricsSetCacheSize(cm.calculateTotalSizeLocked())

	// Trigger cleanup if cache size exceeded
	if cm.calculateTotalSizeLocked() > cm.cfg.MaxSizeBytes() {
		go cm.cleanupLRU()
	}

	return nil
}

// CleanupFailedWrite removes a temporary file after failed write.
func (cm *CacheManager) CleanupFailedWrite(path string) {
	tempPath := path + ".tmp"
	if err := os.Remove(tempPath); err != nil && !os.IsNotExist(err) {
		cm.log.Warn("failed to cleanup temp file",
			zap.String("path", tempPath),
			zap.Error(err),
		)
	}
}

// OpenBinary opens a cached binary file for reading.
func (cm *CacheManager) OpenBinary(path string) (*os.File, error) {
	return os.Open(path)
}

// StartBackgroundTasks starts background goroutines for cleanup and persistence.
func (cm *CacheManager) StartBackgroundTasks(stopCh <-chan struct{}) {
	cm.persistTicker = time.NewTicker(5 * time.Minute)
	cm.cleanupTicker = time.NewTicker(cm.cfg.CleanupInterval)

	// Periodic index persistence
	go func() {
		for {
			select {
			case <-cm.persistTicker.C:
				if err := cm.persistIndex(); err != nil {
					cm.log.Error("failed to persist cache index", zap.Error(err))
				}
			case <-stopCh:
				return
			}
		}
	}()

	// Periodic cleanup
	go func() {
		for {
			select {
			case <-cm.cleanupTicker.C:
				cm.cleanup()
			case <-stopCh:
				return
			}
		}
	}()

	cm.log.Info("background tasks started")
}

// Stop gracefully stops the cache manager.
func (cm *CacheManager) Stop(ctx context.Context) error {
	cm.mu.Lock()
	defer cm.mu.Unlock()

	if cm.persistTicker != nil {
		cm.persistTicker.Stop()
	}
	if cm.cleanupTicker != nil {
		cm.cleanupTicker.Stop()
	}

	// Final index persistence
	if err := cm.persistIndex(); err != nil {
		cm.log.Error("failed to persist index on shutdown", zap.Error(err))
		return err
	}

	cm.log.Info("cache manager stopped")
	return nil
}

// cleanup performs cache cleanup (expired entries and LRU eviction).
func (cm *CacheManager) cleanup() {
	startTime := time.Now()

	cm.index.mu.Lock()
	defer cm.index.mu.Unlock()

	// Remove expired entries
	expiredCount := 0
	for key, entry := range cm.index.entries {
		if time.Now().After(entry.ExpiresAt) {
			cm.removeEntryLocked(entry)
			delete(cm.index.entries, key)
			expiredCount++
			metricsIncCacheEvictions("expired")
		}
	}

	// Check total size and evict if needed
	totalSize := cm.calculateTotalSizeLocked()
	lruCount := 0

	if totalSize > cm.cfg.MaxSizeBytes() {
		lruCount = cm.evictLRULocked(totalSize - cm.cfg.MaxSizeBytes())
	}

	// Update metrics
	metricsSetCacheEntries(len(cm.index.entries))
	metricsSetCacheSize(cm.calculateTotalSizeLocked())

	if expiredCount > 0 || lruCount > 0 {
		cm.log.Info("cache cleanup completed",
			zap.Int("expired_removed", expiredCount),
			zap.Int("lru_removed", lruCount),
			zap.Duration("duration", time.Since(startTime)),
			zap.Int("entries_remaining", len(cm.index.entries)),
		)
	}
}

// cleanupLRU performs LRU eviction when cache size exceeds limit.
func (cm *CacheManager) cleanupLRU() {
	cm.index.mu.Lock()
	defer cm.index.mu.Unlock()

	totalSize := cm.calculateTotalSizeLocked()
	if totalSize <= cm.cfg.MaxSizeBytes() {
		return
	}

	excessSize := totalSize - cm.cfg.MaxSizeBytes()
	count := cm.evictLRULocked(excessSize)

	metricsSetCacheEntries(len(cm.index.entries))
	metricsSetCacheSize(cm.calculateTotalSizeLocked())

	cm.log.Info("LRU eviction completed",
		zap.Int("entries_removed", count),
		zap.Int64("excess_size_mb", excessSize/1024/1024),
	)
}

// evictLRULocked evicts entries from the back of the LRU list until enough space is freed.
// Must be called with index.mu locked.
func (cm *CacheManager) evictLRULocked(targetFreeSize int64) int {
	freedSize := int64(0)
	count := 0

	for freedSize < targetFreeSize && cm.index.lruList.Len() > 0 {
		// Get least recently used entry (back of list)
		elem := cm.index.lruList.Back()
		if elem == nil {
			break
		}

		key := elem.Value.(string)
		entry, found := cm.index.entries[key]
		if !found {
			cm.index.lruList.Remove(elem)
			delete(cm.index.lruMap, key)
			continue
		}

		// Remove entry
		cm.removeEntryLocked(entry)
		delete(cm.index.entries, key)

		freedSize += entry.FileSize
		count++
		metricsIncCacheEvictions("lru")
	}

	return count
}

// removeEntryLocked removes a cache entry (file and index).
// Must be called with index.mu locked.
func (cm *CacheManager) removeEntryLocked(entry *CacheEntry) {
	// Remove file
	if err := os.Remove(entry.FilePath); err != nil && !os.IsNotExist(err) {
		cm.log.Warn("failed to remove cache file",
			zap.String("path", entry.FilePath),
			zap.Error(err),
		)
	}

	// Remove from LRU tracking
	if elem, ok := cm.index.lruMap[entry.Key]; ok {
		cm.index.lruList.Remove(elem)
		delete(cm.index.lruMap, entry.Key)
	}
}

// calculateTotalSizeLocked calculates total cache size.
// Must be called with index.mu locked.
func (cm *CacheManager) calculateTotalSizeLocked() int64 {
	total := int64(0)
	for _, entry := range cm.index.entries {
		total += entry.FileSize
	}
	return total
}

// getCacheFilePath returns the hierarchical file path for a cache key.
func (cm *CacheManager) getCacheFilePath(key string) string {
	// Use first 4 characters for two-level directory hierarchy
	// Example: a3f5d8c2... -> a3/f5/a3f5d8c2e9b1f4a7.bin
	return filepath.Join(
		cm.cfg.Dir,
		key[:2],
		key[2:4],
		key+".bin",
	)
}

// loadIndex loads the cache index from disk.
func (cm *CacheManager) loadIndex() error {
	file, err := os.Open(cm.indexPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil // No index file yet
		}
		return err
	}
	defer file.Close()

	var indexData struct {
		Version int                    `json:"version"`
		Entries map[string]*CacheEntry `json:"entries"`
	}

	if err := json.NewDecoder(file).Decode(&indexData); err != nil {
		return err
	}

	// Validate entries and rebuild LRU list
	validEntries := 0
	entries := make([]*CacheEntry, 0, len(indexData.Entries))

	for key, entry := range indexData.Entries {
		// Check if file exists
		if _, err := os.Stat(entry.FilePath); os.IsNotExist(err) {
			cm.log.Warn("cache file missing, removing from index",
				zap.String("key", key),
				zap.String("path", entry.FilePath),
			)
			continue
		}

		cm.index.entries[key] = entry
		entries = append(entries, entry)
		validEntries++
	}

	// Sort by access time for LRU list (most recent first)
	for i := 0; i < len(entries); i++ {
		for j := i + 1; j < len(entries); j++ {
			if entries[i].AccessedAt.Before(entries[j].AccessedAt) {
				entries[i], entries[j] = entries[j], entries[i]
			}
		}
	}

	// Rebuild LRU list
	for _, entry := range entries {
		elem := cm.index.lruList.PushBack(entry.Key)
		cm.index.lruMap[entry.Key] = elem
	}

	cm.log.Info("cache index loaded",
		zap.Int("valid_entries", validEntries),
		zap.Int("invalid_entries", len(indexData.Entries)-validEntries),
	)

	return nil
}

// persistIndex saves the cache index to disk.
func (cm *CacheManager) persistIndex() error {
	cm.index.mu.RLock()
	defer cm.index.mu.RUnlock()

	indexData := struct {
		Version int                    `json:"version"`
		Entries map[string]*CacheEntry `json:"entries"`
	}{
		Version: 1,
		Entries: cm.index.entries,
	}

	// Write to temp file first
	tempPath := cm.indexPath + ".tmp"
	file, err := os.Create(tempPath)
	if err != nil {
		return fmt.Errorf("failed to create temp index file: %w", err)
	}

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(indexData); err != nil {
		file.Close()
		return fmt.Errorf("failed to encode index: %w", err)
	}

	if err := file.Close(); err != nil {
		return fmt.Errorf("failed to close temp index file: %w", err)
	}

	// Atomic rename
	if err := os.Rename(tempPath, cm.indexPath); err != nil {
		return fmt.Errorf("failed to rename index file: %w", err)
	}

	return nil
}
