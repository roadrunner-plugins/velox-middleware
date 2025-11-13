# Velox Middleware Plugin

A RoadRunner middleware plugin that offloads Velox binary builds from PHP workers to Go, enabling non-blocking builds
with direct file streaming to clients.

## Overview

The Velox middleware plugin intercepts HTTP responses from PHP workers that contain build requests, immediately releases
the worker, delegates the build to a Velox server, and streams the resulting binary directly to the HTTP client.

## Features

- **Non-blocking PHP Workers**: Workers are freed immediately after sending build parameters (< 100ms vs ~60s)
- **Binary Caching**: SHA-256 content-based caching with configurable size, count, and age limits
- **Smart Eviction**: Multiple eviction policies (LRU, LFU, FIFO) for optimal cache management
- **Direct File Streaming**: Binaries stream directly from filesystem to client using efficient 10MB chunking
- **Retry Logic**: Configurable exponential backoff for Velox server communication
- **Context Propagation**: Client disconnects properly cancel ongoing builds
- **Security**: Path traversal protection for file streaming
- **Efficient Buffering**: Smart buffer allocation based on file size

## Configuration

Add to your `.rr.yaml`:

```yaml
http:
  address: '0.0.0.0:8080'
  middleware:
    - velox // Register middleware

velox:
  # Velox build server endpoint (required)
  server_url: "http://127.0.0.1:9000"

  # Build timeout - maximum time to wait for build completion (default: 5m)
  build_timeout: "5m"

  # Request timeout - HTTP timeout for Velox communication (default: 180s)
  request_timeout: "180s"

  # Retry configuration
  retry:
    # Maximum retry attempts (default: 3)
    max_attempts: 3

    # Initial delay before first retry (default: 1s)
    initial_delay: "1s"

    # Maximum delay between retries (default: 10s)
    max_delay: "10s"

    # Backoff multiplier for exponential backoff (default: 2.0)
    backoff_multiplier: 2.0

  # Binary cache configuration (optional)
  cache:
    # Enable caching (default: false)
    enabled: true

    # Cache directory (default: ./cache/velox)
    directory: "/var/cache/velox"

    # Maximum total cache size in GB, 0 = unlimited (default: 0)
    max_size_gb: 50

    # Maximum number of cached binaries, 0 = unlimited (default: 0)
    max_entries: 1000

    # Maximum age for cache entries in days, 0 = never expire (default: 0)
    max_age_days: 30

    # Cleanup interval, 0 = disabled (default: 1h)
    cleanup_interval: "1h"

    # Eviction policy: lru, lfu, or fifo (default: lru)
    eviction_policy: "lru"

    # Access metadata update interval, 0 = immediate (default: 5m)
    access_update_interval: "5m"
```

### Configuration Options

#### Server Configuration

- **`server_url`** (required): Velox build server endpoint URL
- **`build_timeout`**: Maximum time to wait for build completion (default: 5m)
- **`request_timeout`**: HTTP timeout for Velox communication (default: 180s)

#### Retry Configuration

- **`max_attempts`**: Maximum number of retry attempts (default: 3)
- **`initial_delay`**: Initial delay before first retry (default: 1s)
- **`max_delay`**: Maximum delay between retries (default: 10s)
- **`backoff_multiplier`**: Exponential backoff multiplier (default: 2.0)

#### Cache Configuration

- **`enabled`**: Enable or disable binary caching (default: false)
- **`directory`**: Directory for cached binaries (default: ./cache/velox)
- **`max_size_gb`**: Maximum total cache size in gigabytes, 0 = unlimited (default: 0)
- **`max_entries`**: Maximum number of cached binaries, 0 = unlimited (default: 0)
- **`max_age_days`**: Maximum age for cache entries in days, 0 = never expire (default: 0)
- **`cleanup_interval`**: How often to run cache cleanup, 0 = disabled (default: 1h)
- **`eviction_policy`**: Strategy for cache eviction (default: lru)
    - `lru`: Least Recently Used - removes entries that haven't been accessed in the longest time
    - `lfu`: Least Frequently Used - removes entries with the lowest access count
    - `fifo`: First In First Out - removes the oldest created entries first
- **`access_update_interval`**: How often to update access statistics, 0 = immediate (default: 5m)
    - Higher values reduce disk I/O but access tracking is less accurate

## PHP Integration

### Triggering a Velox Build

From your PHP worker, return a response with the `X-Velox-Build` header and a JSON payload:

```php
<?php

use Spiral\RoadRunner\Http\Request;
use Spiral\RoadRunner\Http\Response;

// Build request parameters
$buildRequest = [
    'request_id' => 'b66d5617-64dd-419b-a68b-b002938320ab',
    'target_platform' => [
        'os' => 'windows',
        'arch' => 'amd64',
    ],
    'rr_version' => 'v2025.1.2',
    'plugins' => [
        [
            'module_name' => 'github.com/roadrunner-server/http/v5',
            'tag' => 'v5.2.7',
        ],
        [
            'module_name' => 'github.com/roadrunner-server/grpc/v5',
            'tag' => 'v5.1.0',
        ],
    ],
];

// Return response with Velox header
return new Response(
    status: 200,
    headers: [
        'X-Velox-Build' => 'true',
        'Content-Type' => 'application/json',
    ],
    body: json_encode($buildRequest)
);
```

### Response Flow

1. PHP worker sends response with `X-Velox-Build` header
2. Velox middleware intercepts the response
3. Worker is immediately released back to the pool
4. **Middleware checks cache for matching build**
5. **If cache hit**: Binary streams immediately from cache (~10ms)
6. **If cache miss**: Middleware sends build request to Velox server
7. Velox server builds the binary and returns file path
8. Binary is stored in cache (async, non-blocking)
9. Middleware streams binary directly to HTTP client

## Binary Caching

### How It Works

The cache system uses **content-based hashing** to identify identical builds:

1. **Hash Calculation**: A SHA-256 hash is computed from:
    - RoadRunner version
    - Target platform (OS + architecture)
    - All plugins (sorted by module name and tag)

2. **Cache Lookup**: On build request, the hash is calculated and checked against cached entries

3. **Cache Storage**: Successful builds are stored with paired files:
    - `{hash}.bin` - The binary file
    - `{hash}.json` - Metadata (creation time, access stats, build info)

4. **Cache Management**: Periodic cleanup enforces size, count, and age limits using the configured eviction policy

### Cache Storage Structure

```
/var/cache/velox/
├── a3f5b8c9d2e1...f9a0.bin     # Cached binary
├── a3f5b8c9d2e1...f9a0.json    # Metadata
├── b4c6d9e1f2a3...a1b2.bin
└── b4c6d9e1f2a3...a1b2.json
```

### Metadata Example

```json
{
  "version": "1.0",
  "hash": "a3f5b8c9d2e1f4a6b7c8d9e0f1a2b3c4d5e6f7a8b9c0d1e2f3a4b5c6d7e8f9a0",
  "binary_file": "a3f5b8c9d2e1...f9a0.bin",
  "file_size": 45678912,
  "rr_version": "2024.1.0",
  "platform": {
    "os": "linux",
    "arch": "amd64"
  },
  "plugins": [
    {
      "module_name": "github.com/roadrunner-server/http/v5",
      "tag": "v5.2.7"
    }
  ],
  "created_at": 1699889234,
  "last_accessed": 1699989234,
  "access_count": 15,
  "build_duration_ms": 45000
}
```

### Cache Performance

- **Cache Hit**: ~10ms (file stat + metadata load)
- **Cache Miss**: ~60s (Velox build time)
- **Cache Speedup**: ~6000x faster for cached builds
- **Memory Overhead**: ~2KB per cached entry (metadata)
- **Disk Usage**: Depends on binary sizes (typically 40-80MB per binary)

### Cache Eviction Strategies

**LRU (Least Recently Used)** - Default, recommended for most use cases

- Keeps frequently accessed builds in cache
- Good for environments with recurring build patterns

**LFU (Least Frequently Used)**

- Prioritizes builds that are accessed many times
- Best for identifying truly popular builds vs one-time requests

**FIFO (First In First Out)**

- Simplest strategy, removes oldest entries
- Predictable behavior but may evict frequently-used entries

### Cache Safety

- **Atomic Operations**: All writes use temp file + atomic rename
- **Graceful Degradation**: Cache failures never block build requests
- **No Lock Contention**: Each binary has independent metadata file
- **Orphan Cleanup**: Automatic removal of binaries without metadata
- **Corruption Handling**: Corrupted metadata files are automatically removed

## Protocol

### Build Request Format

```json
{
  "request_id": "b66d5617-64dd-419b-a68b-b002938320ab",
  "target_platform": {
    "os": "windows",
    "arch": "amd64"
  },
  "rr_version": "v2025.1.2",
  "plugins": [
    {
      "module_name": "github.com/roadrunner-server/http/v5",
      "tag": "v5.2.7"
    }
  ]
}
```

### Velox Server Response Format

**Success:**

```json
{
  "success": true,
  "path": "/tmp/velox/builds/roadrunner-windows-amd64-abc123.exe",
  "build_id": "abc123def456",
  "duration_ms": 58234
}
```

**Failure:**

```json
{
  "success": false,
  "error": "failed to compile plugin: github.com/invalid/plugin",
  "code": "BUILD_ERROR"
}
```

## Architecture

### Request Flow

```
┌──────────┐     ┌──────────┐     ┌────────┐     ┌───────┐
│  Client  │────▶│ RR HTTP  │────▶│  PHP   │────▶│ Velox │
│          │     │          │     │ Worker │     │Plugin │
└──────────┘     └──────────┘     └────────┘     └───────┘
                                       │              │
                                       │Released      │Cache Check
                                       │immediately   │     │
                                       ▼              ▼     ▼
                                  ┌──────────────────────────┐
                                  │  Cache Hit?              │
                                  └──────────────────────────┘
                                       │              │
                                  Yes  │              │ No
                                       ▼              ▼
                                  ┌────────┐   ┌──────────┐
                                  │ Stream │   │  Build   │
                                  │ Cached │   │  with    │
                                  │ Binary │   │  Velox   │
                                  └────────┘   └──────────┘
                                                     │
                                                     ▼
                                                ┌────────┐
                                                │ Store  │
                                                │  in    │
                                                │ Cache  │
                                                └────────┘
                                                     │
                                                     ▼
                                                ┌────────┐
                                                │ Stream │
                                                │ Binary │
                                                └────────┘
```

### Key Design Decisions

**Binary Caching**: SHA-256 content-based hashing ensures identical builds are never rebuilt unnecessarily.
**Per-Binary Metadata**: Independent JSON files eliminate lock contention and enable concurrent cache operations.
**Writer Pooling**: Response writers are pooled to reduce garbage collection pressure during high traffic.
**Chunked Streaming**: Files are streamed in 10MB chunks with automatic flushing for progressive downloads.
**Smart Buffer Allocation**: Small files use exact-size buffers, large files use fixed 10MB buffers to prevent memory
spikes.
**Context Propagation**: Client disconnects trigger context cancellation, aborting ongoing Velox requests and cache
operations.
**Retry with Backoff**: Failed Velox requests retry with exponential backoff up to configured maximum.
**Batched Access Updates**: Access statistics are batched to minimize disk I/O on cache hits.

## License

MIT License - see LICENSE file for details
