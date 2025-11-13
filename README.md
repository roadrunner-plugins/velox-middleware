# Velox Plugin

A high-performance RoadRunner HTTP middleware plugin that intercepts Velox binary build requests from PHP workers, manages
intelligent caching with LRU eviction, and streams binaries directly to clients without blocking expensive PHP worker
processes.

## Overview

The Velox middleware plugin solves a critical performance problem: PHP workers blocking for 1+ minutes during Velox
binary builds. This plugin intercepts special HTTP responses containing build requests, immediately releases the PHP
worker, and handles the entire build-cache-stream lifecycle in Go.

### Key Features

- **Worker Efficiency**: PHP workers released immediately (< 100ms) instead of blocking for minutes
- **Intelligent Caching**: SHA256-based content-addressable cache with configurable TTL and LRU eviction
- **Concurrent Builds**: Semaphore-limited concurrent builds with configurable max (default: 5)
- **Retry Logic**: Exponential backoff retry for transient Velox server failures
- **Comprehensive Metrics**: 20+ Prometheus metrics for monitoring cache effectiveness, build performance, and errors
- **Graceful Shutdown**: Waits for active builds to complete before shutdown
- **Production Ready**: Extensive error handling, structured logging, and observability
- **Automatic Registration**: Middleware automatically discovered and registered by HTTP plugin

## Architecture

```
┌─────────────┐
│ PHP Worker  │ (Sends build request with X-Velox-Build header)
└──────┬──────┘
       │
       ▼
┌──────────────────────────────────────────────────────────┐
│                     Velox Plugin                         │
│  (HTTP Middleware - automatically registered)            │
│                                                           │
│  1. Parse build request from response body               │
│  2. Generate cache key (SHA256 of normalized config)     │
│  3. Check cache (if not force_rebuild)                   │
│     ├─ HIT  → Stream cached binary to client             │
│     └─ MISS → Acquire semaphore slot                     │
│                └─ Call Velox server                      │
│                   └─ Stream to client + cache            │
└──────────────────────────────────────────────────────────┘
       │
       ▼
┌─────────────┐
│   Client    │ (Receives binary stream)
└─────────────┘
```

## Configuration

### Plugin Registration

This plugin can be registered with RoadRunner's HTTP plugin in two ways:

#### 1. Manual Registration (Recommended)

Explicitly list the plugin in HTTP middleware configuration:

```yaml
http:
  address: 0.0.0.0:8080
  middleware: ["velox"]  # Explicit middleware registration

velox:
  server_url: "http://127.0.0.1:9000"
```

This gives you full control over middleware order and ensures the plugin is only active when explicitly enabled.

#### 2. Automatic Discovery

If no middleware list is specified, the HTTP plugin discovers all plugins implementing the middleware interface:

```yaml
http:
  address: 0.0.0.0:8080
  # No middleware: [] - automatic discovery

velox:
  server_url: "http://127.0.0.1:9000"
```

**Note:** For production environments, manual registration is recommended for predictable behavior.

### Minimal Configuration

```yaml
http:
  address: 0.0.0.0:8080
  middleware: ["velox"]

velox:
  server_url: "http://127.0.0.1:9000"
```

### Full Configuration

```yaml
http:
  address: 0.0.0.0:8080
  middleware: ["gzip", "velox", "sendfile"]  # Middleware execution order

velox:
  # Velox server endpoint (required)
  server_url: "http://velox.internal.company.com:9000"

  # Build timeout (default: 5m)
  build_timeout: "10m"

  # Maximum concurrent builds (default: 5)
  max_concurrent_builds: 10

  # Request timeout for Velox server communication (default: 10s)
  request_timeout: "30s"

  # Cache configuration
  cache:
    # Enable caching (default: true)
    enabled: true

    # Cache directory path (default: ./.rr-cache/velox-binaries)
    dir: "/var/cache/roadrunner/velox"

    # Cache TTL in days (default: 7)
    ttl_days: 14

    # Maximum cache size in MB (default: 5000 = 5GB)
    # When exceeded, oldest binaries are removed (LRU)
    max_size_mb: 10000

    # Cleanup interval - how often to check for expired entries (default: 1h)
    cleanup_interval: "30m"

  # Retry configuration
  retry:
    max_attempts: 5
    initial_delay: "2s"
    max_delay: "30s"
```

### Cache Disabled

```yaml
velox:
  server_url: "http://127.0.0.1:9000"

  cache:
    enabled: false
```

### Integration with Other Plugins

Middleware execution order is controlled by the `middleware` list:

```yaml
# .rr.yaml
version: "3"

# HTTP plugin with explicit middleware registration
http:
  address: 0.0.0.0:8080
  
  # Middleware execution order (left to right)
  # Request flow: gzip → velox → sendfile → PHP workers
  middleware: ["gzip", "velox", "sendfile"]

# Velox plugin configuration  
velox:
  server_url: "http://velox-server:9000"
  max_concurrent_builds: 10
  cache:
    enabled: true
    max_size_mb: 10000

# Metrics plugin for Prometheus integration
metrics:
  address: 0.0.0.0:2112

# Other plugins work normally
rpc:
  listen: tcp://127.0.0.1:6001

# Logs plugin
logs:
  mode: production
  level: info
```

### Middleware Order Matters

```yaml
# Example 1: Velox before sendfile
http:
  middleware: ["velox", "sendfile"]
# If X-Velox-Build header present → Velox handles it
# Otherwise → request passes to sendfile → PHP workers

# Example 2: Sendfile before velox (not recommended)
http:
  middleware: ["sendfile", "velox"]
# Sendfile checks response first, then velox
# This may cause issues if both headers are present
```

**Best Practice:** Place `velox` middleware early in the chain since it intercepts requests before they reach PHP workers.

## PHP Integration

### Building a Binary

```php
<?php

namespace App\Http\Controllers;

use Illuminate\Http\JsonResponse;
use Ramsey\Uuid\Uuid;

class VeloxBuildController extends Controller
{
    public function build(): JsonResponse
    {
        $buildRequest = [
            'request_id' => Uuid::uuid4()->toString(),
            'force_rebuild' => false, // Use cache if available
            'target_platform' => [
                'os' => 'linux',
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

        return response()
            ->json($buildRequest)
            ->header('X-Velox-Build', 'true');
    }
}
```

### Force Rebuild

Set `force_rebuild: true` to bypass cache and always build from Velox server:

```php
$buildRequest = [
    'request_id' => Uuid::uuid4()->toString(),
    'force_rebuild' => true, // Always build fresh
    // ... rest of config
];
```

## Cache Architecture

### Cache Key Generation

Cache keys are generated using SHA256 hash of normalized build configuration:

1. **Extract Fields**: `rr_version`, `os`, `arch`, `plugins[]`
2. **Normalize**: Sort plugins alphabetically, lowercase all strings, remove whitespace
3. **Hash**: Compute SHA256 hash of canonical JSON representation
4. **Truncate**: Use first 16 characters as cache key

Example:

```
Config: RR v2025.1.2, linux/amd64, http+grpc
Cache Key: a3f5d8c2e9b1f4a7
```

### Filesystem Structure

Hierarchical directory structure prevents too many files in single directory:

```
.rr-cache/velox-binaries/
├── index.json                          # Cache metadata index
├── a3/                                 # First 2 chars of hash
│   └── f5/                            # Next 2 chars of hash
│       └── a3f5d8c2e9b1f4a7.bin      # Cached binary
├── b7/
│   └── e2/
│       └── b7e2c9d4f1a8e3b6.bin
└── c9/
    └── f8/
        └── c9f8a3e1b7d2f5a4.bin
```

### Cache Index

In-memory index with LRU tracking for fast lookups:

```json
{
  "version": 1,
  "entries": {
    "a3f5d8c2e9b1f4a7": {
      "file_path": "a3/f5/a3f5d8c2e9b1f4a7.bin",
      "file_size": 52428800,
      "created_at": "2025-11-13T10:30:00Z",
      "expires_at": "2025-11-20T10:30:00Z",
      "accessed_at": "2025-11-13T15:45:00Z",
      "build_config": {
        "rr_version": "v2025.1.2",
        "os": "linux",
        "arch": "amd64",
        "plugins": [
          ...
        ]
      }
    }
  }
}
```

### Cache Eviction

**Time-based (TTL):**

- Default: 7 days from creation
- Configurable via `cache.ttl_days`
- Background goroutine removes expired entries every `cleanup_interval`

**Size-based (LRU):**

- Max cache size: configurable (default: 5GB)
- When exceeded, remove least recently used entries
- Access time updated on cache hits

## Metrics

### Cache Metrics

```promql
# Cache hit rate (last 1h)
sum(rate(velox_cache_hits_total[1h])) / 
(sum(rate(velox_cache_hits_total[1h])) + sum(rate(velox_cache_misses_total[1h])))

# Cache size growth rate (bytes per hour)
rate(velox_cache_size_bytes[1h]) * 3600

# Cache evictions by reason
rate(velox_cache_evictions_total[5m])
```

### Build Metrics

```promql
# P50/P95/P99 build duration
histogram_quantile(0.50, rate(velox_build_duration_seconds_bucket[5m]))
histogram_quantile(0.95, rate(velox_build_duration_seconds_bucket[5m]))
histogram_quantile(0.99, rate(velox_build_duration_seconds_bucket[5m]))

# Active builds
velox_builds_active

# Build success rate
sum(rate(velox_builds_total{status="success"}[5m])) / 
sum(rate(velox_builds_total[5m]))
```

### Error Metrics

```promql
# Error rate by type
sum(rate(velox_errors_total[5m])) by (type)

# Build timeout rate
rate(velox_builds_total{status="timeout"}[5m])
```

### Available Metrics

**Cache:**

- `velox_cache_hits_total` - Total cache hits
- `velox_cache_misses_total` - Total cache misses
- `velox_cache_evictions_total{reason}` - Evictions by reason (expired/lru)
- `velox_cache_entries_count` - Current cache entries
- `velox_cache_size_bytes` - Current cache size
- `velox_cache_access_age_seconds` - Most recent cache access age

**Builds:**

- `velox_builds_total{status,cache_status}` - Total builds by status
- `velox_force_rebuilds_total` - Total force rebuilds
- `velox_builds_active` - Active builds
- `velox_build_duration_seconds{os,arch}` - Build duration histogram
- `velox_binary_size_bytes{os,arch}` - Binary size histogram

**Errors:**

- `velox_errors_total{type}` - Errors by type
- `velox_retries_total{reason}` - Retry attempts by reason

**Streaming:**

- `velox_stream_ttfb_seconds{cache_status}` - Time to first byte
- `velox_client_disconnects_total{stage}` - Client disconnects by stage

## Response Headers

### Successful Cache Hit

```
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Content-Disposition: attachment; filename="roadrunner-linux-amd64"
Content-Length: 52428800
X-Build-Request-ID: b66d5617-64dd-419b-a68b-b002938320ab
X-Cache-Status: HIT
X-Cache-Age: 3600

[binary data stream]
```

### Successful Cache Miss

```
HTTP/1.1 200 OK
Content-Type: application/octet-stream
Content-Disposition: attachment; filename="roadrunner-windows-amd64.exe"
X-Build-Request-ID: b66d5617-64dd-419b-a68b-b002938320ab
X-Cache-Status: MISS
X-Build-Time: 58.3

[binary data stream]
```

### Error Responses

**Validation Error (400):**

```json
{
  "error": "invalid_request",
  "message": "missing required field: rr_version",
  "request_id": "b66d5617-64dd-419b-a68b-b002938320ab"
}
```

**Build Failure (502):**

```json
{
  "error": "build_failed",
  "message": "velox server returned: plugin version conflict",
  "request_id": "b66d5617-64dd-419b-a68b-b002938320ab"
}
```

**Timeout (504):**

```json
{
  "error": "timeout",
  "message": "build exceeded 5m timeout",
  "request_id": "b66d5617-64dd-419b-a68b-b002938320ab"
}
```

## Performance Characteristics

### Worker Release Time

- **Target**: < 100ms
- **Actual**: Typically 20-50ms (parse request + cache lookup + spawn goroutine)
- PHP workers freed immediately, no 1-minute blocking

### Cache Hit Response Time

- **Typical**: < 1 second
- **Operations**: Cache lookup (O(1)), file open, stream to client
- 70-80% cache hit rate expected in production

### Cache Miss Build Time

- **Typical**: 30-90 seconds (depends on Velox server, build complexity)
- **Concurrent**: Up to `max_concurrent_builds` simultaneous builds
- Retry logic handles transient failures

### Resource Usage

- **Memory**: ~5MB for cache index (10,000 entries)
- **Disk**: Configurable (default: 5GB max)
- **Goroutines**: 1 per active build + 2 background tasks

## Error Handling

### Retry Logic

- **Retryable**: Connection timeouts, 5xx server errors
- **Non-retryable**: Validation errors (4xx), context cancellation
- **Strategy**: Exponential backoff (1s → 2s → 4s → 8s → 10s max)
- **Max attempts**: Configurable (default: 3)

### Cache Failures

- Cache read failures are logged but don't fail the request
- Cache write failures are logged but don't prevent streaming to client
- Partial builds are never cached (atomic write with temp file + rename)

### Graceful Degradation

- If cache disabled or unavailable, all requests go to Velox server
- If Velox server unavailable, returns appropriate error to client
- Client disconnects cancel ongoing builds and free resources

## Logging

### Log Levels

**DEBUG**: Cache operations, streaming progress
**INFO**: Build lifecycle, cache hits/misses, cleanup operations
**WARN**: Cache failures, retry attempts, client disconnects
**ERROR**: Build failures, configuration issues, unrecoverable errors

### Structured Logging Examples

```
INFO    build request received
        request_id=b66d5617-64dd-419b-a68b-b002938320ab
        os=linux arch=amd64 rr_version=v2025.1.2
        plugins_count=2 force_rebuild=false

INFO    cache hit
        request_id=b66d5617-64dd-419b-a68b-b002938320ab
        cache_key=a3f5d8c2e9b1f4a7
        cache_age=1h15m file_size=52428800

INFO    build completed
        request_id=b66d5617-64dd-419b-a68b-b002938320ab
        build_duration=58.3s bytes_written=52428800

INFO    cache cleanup completed
        expired_removed=5 lru_removed=2
        duration=125ms entries_remaining=47
```

## Troubleshooting

### High Cache Miss Rate

**Symptoms**: `velox_cache_hits_total / velox_cache_misses_total < 0.5`

**Causes**:

- TTL too short (increase `cache.ttl_days`)
- Cache size too small (increase `cache.max_size_mb`)
- Frequent `force_rebuild` requests
- High variety of build configurations

### Build Timeouts

**Symptoms**: `velox_builds_total{status="timeout"}` increasing

**Solutions**:

- Increase `build_timeout` (default: 5m)
- Check Velox server performance
- Review network connectivity

### Cache Size Growing Too Fast

**Symptoms**: `velox_cache_evictions_total{reason="lru"}` increasing rapidly

**Solutions**:

- Increase `cache.max_size_mb`
- Decrease `cache.ttl_days`
- Consider using dedicated disk/partition

### PHP Workers Still Blocking

**Symptoms**: PHP workers not immediately freed

**Causes**:

- `X-Velox-Build` header not set to `"true"` (case-sensitive)
- Invalid JSON in response body
- Middleware not properly registered

## License

MIT License

## Contributing

Contributions welcome! Please open an issue or pull request.

## Support

For issues, questions, or feature requests, please open an issue on GitHub.
