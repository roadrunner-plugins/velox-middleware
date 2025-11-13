package veloxmiddleware

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// VeloxClient handles communication with the Velox server.
type VeloxClient struct {
	serverURL  string
	httpClient *http.Client
	cfg        *Config
	log        *zap.Logger
}

// NewVeloxClient creates a new Velox client.
func NewVeloxClient(cfg *Config, log *zap.Logger) (*VeloxClient, error) {
	client := &VeloxClient{
		serverURL: cfg.ServerURL,
		cfg:       cfg,
		log:       log.Named("velox_client"),
		httpClient: &http.Client{
			// Use BuildTimeout for HTTP client since builds are long-running operations
			// The individual HTTP request timeout must be >= the build timeout
			Timeout: cfg.BuildTimeout + (30 * time.Second), // Add buffer for network overhead
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 10,
				IdleConnTimeout:     90 * time.Second,
				DisableCompression:  true, // Binary data shouldn't be compressed
			},
		},
	}

	return client, nil
}

// Build sends a build request to the Velox server with retry logic.
func (c *VeloxClient) Build(ctx context.Context, req *BuildRequest) (*http.Response, error) {
	var lastErr error
	delay := c.cfg.Retry.InitialDelay

	for attempt := 1; attempt <= c.cfg.Retry.MaxAttempts; attempt++ {
		resp, err := c.doBuild(ctx, req)
		if err == nil {
			return resp, nil
		}

		lastErr = err

		// Don't retry on context cancellation
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}

		// Don't retry on client errors (4xx), except for 409 which might resolve
		if resp != nil && resp.StatusCode >= 400 && resp.StatusCode < 500 {
			// 409 (Conflict) means build already in progress - we should retry with longer delay
			if resp.StatusCode == 409 {
				c.log.Info("build already in progress, will retry with longer delay",
					zap.String("request_id", req.RequestID),
					zap.Int("attempt", attempt),
				)
				// Use a longer delay for 409 to give the existing build time to complete
				delay = c.cfg.Retry.MaxDelay
			} else {
				// Other 4xx errors are non-retryable
				return resp, err
			}
		}

		// Last attempt, don't wait
		if attempt == c.cfg.Retry.MaxAttempts {
			break
		}

		// Log retry
		c.log.Warn("build attempt failed, retrying",
			zap.String("request_id", req.RequestID),
			zap.Int("attempt", attempt),
			zap.Int("max_attempts", c.cfg.Retry.MaxAttempts),
			zap.Duration("retry_delay", delay),
			zap.Error(err),
		)

		metricsIncRetries("velox_server")

		// Wait before retry
		select {
		case <-time.After(delay):
			// Exponential backoff with max delay cap
			delay = time.Duration(math.Min(float64(delay*2), float64(c.cfg.Retry.MaxDelay)))
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}

	return nil, fmt.Errorf("build failed after %d attempts: %w", c.cfg.Retry.MaxAttempts, lastErr)
}

// doBuild performs a single build request to the Velox server.
func (c *VeloxClient) doBuild(ctx context.Context, req *BuildRequest) (*http.Response, error) {
	// Prepare request body (Connect RPC expects JSON)
	reqBody := map[string]interface{}{
		"request_id": req.RequestID, // Required by Velox server
		"target_platform": map[string]string{
			"os":   req.TargetPlatform.OS,
			"arch": req.TargetPlatform.Arch,
		},
		"rr_version":    req.RRVersion,
		"plugins":       req.Plugins,
		"force_rebuild": req.ForceRebuild,
	}

	bodyBytes := &bytes.Buffer{}
	if err := json.NewEncoder(bodyBytes).Encode(reqBody); err != nil {
		return nil, fmt.Errorf("failed to encode request: %w", err)
	}

	// Create HTTP request
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api.service.v1.BuildService/Build", c.serverURL),
		bodyBytes,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers for Connect RPC
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/octet-stream")

	c.log.Debug("sending build request to Velox",
		zap.String("request_id", req.RequestID),
		zap.String("url", httpReq.URL.String()),
	)

	// Send request
	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}

	c.log.Debug("received response from Velox",
		zap.String("request_id", req.RequestID),
		zap.Int("status_code", resp.StatusCode),
		zap.Int64("content_length", resp.ContentLength),
	)

	return resp, nil
}

// Close closes the HTTP client connections.
func (c *VeloxClient) Close() error {
	c.httpClient.CloseIdleConnections()
	return nil
}
