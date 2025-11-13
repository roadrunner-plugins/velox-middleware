# Velox Middleware Plugin

A RoadRunner middleware plugin that offloads Velox binary builds from PHP workers to Go, enabling non-blocking builds
with direct file streaming to clients.

## Overview

The Velox middleware plugin intercepts HTTP responses from PHP workers that contain build requests, immediately releases
the worker, delegates the build to a Velox server, and streams the resulting binary directly to the HTTP client.

## Features

- **Non-blocking PHP Workers**: Workers are freed immediately after sending build parameters (< 100ms vs ~60s)
- **Direct File Streaming**: Binaries stream directly from filesystem to client using efficient 10MB chunking
- **Retry Logic**: Configurable exponential backoff for Velox server communication
- **Context Propagation**: Client disconnects properly cancel ongoing builds
- **Security**: Path traversal protection for file streaming
- **Efficient Buffering**: Smart buffer allocation based on file size

## Configuration

Add to your `.rr.yaml`:

```yaml
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
```

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
4. Middleware sends build request to Velox server
5. Velox server builds the binary and returns file path
6. Middleware streams binary directly to HTTP client

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
                                       │Released      │
                                       │immediately   │
                                       ▼              ▼
                                  ┌──────────────────────┐
                                  │   Send to Velox      │
                                  │   Build Server       │
                                  └──────────────────────┘
                                              │
                                              ▼
                                  ┌──────────────────────┐
                                  │  Stream Binary File  │
                                  │  to Client           │
                                  └──────────────────────┘
```

### Key Design Decisions

**Writer Pooling**: Response writers are pooled to reduce garbage collection pressure during high traffic.

**Chunked Streaming**: Files are streamed in 10MB chunks with automatic flushing for progressive downloads.

**Smart Buffer Allocation**: Small files use exact-size buffers, large files use fixed 10MB buffers to prevent memory
spikes.

**Context Propagation**: Client disconnects trigger context cancellation, aborting ongoing Velox requests.

**Retry with Backoff**: Failed Velox requests retry with exponential backoff up to configured maximum.

## Error Handling

### HTTP Status Codes

- `400 Bad Request`: Invalid JSON payload from PHP worker
- `502 Bad Gateway`: Velox server unreachable or returned error
- `504 Gateway Timeout`: Build exceeded configured timeout
- `500 Internal Server Error`: Build failed or file streaming error

### Logging

All operations are logged with structured context:

```go
p.log.Info("velox build completed",
zap.String("request_id", buildReq.RequestID),
zap.String("build_id", veloxResp.BuildID),
zap.Int64("duration_ms", veloxResp.DurationMs),
zap.String("file_path", veloxResp.Path),
)
```

## Performance Characteristics

- **Worker Release Time**: < 100ms (vs ~60s for synchronous builds)
- **Memory Usage**: ~10MB per concurrent build request (for buffer)
- **Concurrent Builds**: Limited only by Velox server capacity
- **File Streaming**: Zero-copy streaming from disk to network

## Security

- **Path Traversal Protection**: Rejects file paths containing `..`
- **No Arbitrary File Access**: Only streams files explicitly returned by Velox server
- **Timeout Protection**: All operations bounded by configurable timeouts

## Troubleshooting

### Build requests not detected

**Check:**

- PHP worker sends `X-Velox-Build: true` header
- Response body is valid JSON
- Plugin is enabled in `.rr.yaml`
- Plugin loaded in RoadRunner middleware chain

### Velox server connection failures

**Check:**

- `server_url` is correct and reachable
- Velox server is running and healthy
- Network connectivity between RR and Velox
- Request timeout is sufficient for network latency

### File streaming failures

**Check:**

- Velox server returns absolute file path
- File exists and is readable
- File permissions allow read access
- Disk I/O is not bottlenecked

## Development

### Building

```bash
cd velox-middleware
go mod tidy
go build
```

### Testing

```bash
go test -v ./...
```

### Integration with RoadRunner

Register the plugin in your RoadRunner build:

```go
import "github.com/roadrunner-plugins/velox-middleware"

// In your container initialization
err := container.Register(&velox.Plugin{})
if err != nil {
panic(err)
}
```

## License

MIT License - see LICENSE file for details
