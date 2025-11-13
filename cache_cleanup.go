package velox

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"go.uber.org/zap"
)

const (
	bytesPerGB = 1024 * 1024 * 1024
)

// evictionCandidate represents a cache entry candidate for eviction
type evictionCandidate struct {
	hash         string
	metadataPath string
	binaryPath   string
	fileSize     int64
	createdAt    int64
	lastAccessed int64
	accessCount  int
}

// Cleanup performs cache cleanup based on configured limits
func (cm *CacheManager) Cleanup() error {
	cm.log.Debug("starting cache cleanup")
	startTime := time.Now()

	// Scan cache directory for all metadata files
	candidates, err := cm.scanCacheEntries()
	if err != nil {
		return fmt.Errorf("failed to scan cache entries: %w", err)
	}

	if len(candidates) == 0 {
		cm.log.Debug("no cache entries found")
		return nil
	}

	// Calculate current cache statistics
	var totalSize int64
	for _, candidate := range candidates {
		totalSize += candidate.fileSize
	}

	entriesCount := len(candidates)
	initialCount := entriesCount
	initialSize := totalSize

	cm.log.Debug("cache statistics before cleanup",
		zap.Int("entries", entriesCount),
		zap.Int64("total_size_bytes", totalSize),
		zap.Float64("total_size_gb", float64(totalSize)/bytesPerGB),
	)

	// Collect entries to evict
	toEvict := make([]evictionCandidate, 0)

	// 1. Remove entries older than max age
	if cm.cfg.MaxAgeDays > 0 {
		maxAge := time.Now().Unix() - int64(cm.cfg.MaxAgeDays*24*60*60)
		for i := len(candidates) - 1; i >= 0; i-- {
			if candidates[i].createdAt < maxAge {
				toEvict = append(toEvict, candidates[i])
				totalSize -= candidates[i].fileSize
				candidates = append(candidates[:i], candidates[i+1:]...)
				entriesCount--
			}
		}

		if len(toEvict) > 0 {
			cm.log.Debug("found expired entries",
				zap.Int("count", len(toEvict)),
			)
		}
	}

	// 2. Check if we exceed size limit
	if cm.cfg.MaxSizeGB > 0 {
		maxSizeBytes := int64(cm.cfg.MaxSizeGB * bytesPerGB)
		if totalSize > maxSizeBytes {
			// Sort by eviction policy and remove until under limit
			cm.sortByEvictionPolicy(candidates)

			for len(candidates) > 0 && totalSize > maxSizeBytes {
				candidate := candidates[0]
				candidates = candidates[1:]
				toEvict = append(toEvict, candidate)
				totalSize -= candidate.fileSize
				entriesCount--
			}

			cm.log.Debug("evicting entries due to size limit",
				zap.Float64("max_size_gb", cm.cfg.MaxSizeGB),
				zap.Int("evicted", len(toEvict)),
			)
		}
	}

	// 3. Check if we exceed entry count limit
	if cm.cfg.MaxEntries > 0 && entriesCount > cm.cfg.MaxEntries {
		// Sort by eviction policy if not already sorted
		if cm.cfg.MaxSizeGB <= 0 {
			cm.sortByEvictionPolicy(candidates)
		}

		excess := entriesCount - cm.cfg.MaxEntries
		for i := 0; i < excess && len(candidates) > 0; i++ {
			candidate := candidates[0]
			candidates = candidates[1:]
			toEvict = append(toEvict, candidate)
			totalSize -= candidate.fileSize
			entriesCount--
		}

		cm.log.Debug("evicting entries due to count limit",
			zap.Int("max_entries", cm.cfg.MaxEntries),
			zap.Int("evicted", excess),
		)
	}

	// Perform evictions
	evictedCount := 0
	var evictedSize int64

	for _, candidate := range toEvict {
		if err := cm.evictEntry(candidate); err != nil {
			cm.log.Warn("failed to evict cache entry",
				zap.Error(err),
				zap.String("hash", candidate.hash),
			)
			continue
		}
		evictedCount++
		evictedSize += candidate.fileSize
	}

	duration := time.Since(startTime)

	cm.log.Info("cache cleanup completed",
		zap.Int("initial_entries", initialCount),
		zap.Int("final_entries", entriesCount),
		zap.Int("evicted_entries", evictedCount),
		zap.Int64("initial_size_bytes", initialSize),
		zap.Int64("final_size_bytes", totalSize),
		zap.Int64("freed_bytes", evictedSize),
		zap.Float64("freed_gb", float64(evictedSize)/bytesPerGB),
		zap.Duration("duration", duration),
	)

	return nil
}

// scanCacheEntries scans the cache directory and returns all cache entries
func (cm *CacheManager) scanCacheEntries() ([]evictionCandidate, error) {
	entries, err := os.ReadDir(cm.cacheDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read cache directory: %w", err)
	}

	candidates := make([]evictionCandidate, 0)

	for _, entry := range entries {
		// Skip if not a metadata file
		if !entry.IsDir() && filepath.Ext(entry.Name()) == metadataFileSuffix {
			metadataPath := filepath.Join(cm.cacheDir, entry.Name())

			// Load metadata
			metadata, err := cm.loadMetadata(metadataPath)
			if err != nil {
				cm.log.Warn("failed to load metadata during scan, skipping",
					zap.Error(err),
					zap.String("file", entry.Name()),
				)
				continue
			}

			// Check if binary exists
			binaryPath := filepath.Join(cm.cacheDir, metadata.BinaryFile)
			binaryInfo, err := os.Stat(binaryPath)
			if err != nil {
				cm.log.Warn("binary file missing, removing orphaned metadata",
					zap.String("hash", metadata.Hash),
				)
				_ = os.Remove(metadataPath)
				continue
			}

			candidates = append(candidates, evictionCandidate{
				hash:         metadata.Hash,
				metadataPath: metadataPath,
				binaryPath:   binaryPath,
				fileSize:     binaryInfo.Size(),
				createdAt:    metadata.CreatedAt,
				lastAccessed: metadata.LastAccessed,
				accessCount:  metadata.AccessCount,
			})
		}
	}

	return candidates, nil
}

// sortByEvictionPolicy sorts candidates based on the configured eviction policy
func (cm *CacheManager) sortByEvictionPolicy(candidates []evictionCandidate) {
	switch cm.cfg.EvictionPolicy {
	case "lru": // Least Recently Used
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].lastAccessed < candidates[j].lastAccessed
		})
	case "lfu": // Least Frequently Used
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].accessCount < candidates[j].accessCount
		})
	case "fifo": // First In First Out
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].createdAt < candidates[j].createdAt
		})
	default:
		// Default to LRU
		cm.log.Warn("unknown eviction policy, using LRU",
			zap.String("policy", cm.cfg.EvictionPolicy),
		)
		sort.Slice(candidates, func(i, j int) bool {
			return candidates[i].lastAccessed < candidates[j].lastAccessed
		})
	}
}

// evictEntry removes a cache entry (both binary and metadata)
func (cm *CacheManager) evictEntry(candidate evictionCandidate) error {
	// Delete binary file first
	if err := os.Remove(candidate.binaryPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete binary file: %w", err)
	}

	// Delete metadata file
	if err := os.Remove(candidate.metadataPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete metadata file: %w", err)
	}

	cm.log.Debug("cache entry evicted",
		zap.String("hash", candidate.hash),
		zap.Int64("file_size", candidate.fileSize),
	)

	return nil
}
