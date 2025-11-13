package veloxmiddleware

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// BuildRequest represents the build request from PHP worker.
type BuildRequest struct {
	RequestID      string          `json:"request_id"`
	ForceRebuild   bool            `json:"force_rebuild"`
	TargetPlatform Platform        `json:"target_platform"`
	RRVersion      string          `json:"rr_version"`
	Plugins        []PluginRequest `json:"plugins"`
}

// Platform represents the target platform for the build.
type Platform struct {
	OS   string `json:"os"`   // windows, linux, darwin
	Arch string `json:"arch"` // amd64, arm64
}

// PluginRequest represents a RoadRunner plugin to include in the build.
type PluginRequest struct {
	ModuleName string `json:"module_name"`
	Tag        string `json:"tag"`
}

// Validate validates the build request.
func (br *BuildRequest) Validate() error {
	if br.RequestID == "" {
		return fmt.Errorf("request_id is required")
	}

	if br.RRVersion == "" {
		return fmt.Errorf("rr_version is required")
	}

	if err := br.TargetPlatform.Validate(); err != nil {
		return fmt.Errorf("invalid target_platform: %w", err)
	}

	if len(br.Plugins) == 0 {
		return fmt.Errorf("at least one plugin is required")
	}

	if len(br.Plugins) > 100 {
		return fmt.Errorf("too many plugins (max 100)")
	}

	for i, plugin := range br.Plugins {
		if err := plugin.Validate(); err != nil {
			return fmt.Errorf("invalid plugin at index %d: %w", i, err)
		}
	}

	return nil
}

// Validate validates the platform configuration.
func (p *Platform) Validate() error {
	validOS := map[string]bool{
		"windows": true,
		"linux":   true,
		"darwin":  true,
	}

	validArch := map[string]bool{
		"amd64": true,
		"arm64": true,
	}

	if !validOS[p.OS] {
		return fmt.Errorf("invalid os: %s (must be windows, linux, or darwin)", p.OS)
	}

	if !validArch[p.Arch] {
		return fmt.Errorf("invalid arch: %s (must be amd64 or arm64)", p.Arch)
	}

	return nil
}

// Validate validates the plugin request configuration.
func (pr *PluginRequest) Validate() error {
	if pr.ModuleName == "" {
		return fmt.Errorf("module_name is required")
	}

	if pr.Tag == "" {
		return fmt.Errorf("tag is required")
	}

	// Basic semver validation (v prefix optional)
	tag := strings.TrimPrefix(pr.Tag, "v")
	parts := strings.Split(tag, ".")
	if len(parts) != 3 {
		return fmt.Errorf("invalid tag format: %s (expected semver like v1.2.3)", pr.Tag)
	}

	return nil
}

// GenerateCacheKey generates a deterministic cache key from the build request.
// The cache key is a SHA256 hash of the normalized build configuration.
func (br *BuildRequest) GenerateCacheKey() string {
	normalized := br.normalize()

	// Convert to canonical JSON
	jsonBytes, _ := json.Marshal(normalized)

	// Compute SHA256 hash
	hash := sha256.Sum256(jsonBytes)

	// Return first 16 characters of hex-encoded hash
	return hex.EncodeToString(hash[:])[:16]
}

// normalize creates a normalized representation of the build request for hashing.
// This ensures that identical configurations produce identical cache keys regardless
// of input order or formatting.
func (br *BuildRequest) normalize() normalizedBuildConfig {
	// Sort plugins by module name for consistent hashing
	plugins := make([]normalizedPlugin, len(br.Plugins))
	for i, p := range br.Plugins {
		plugins[i] = normalizedPlugin{
			ModuleName: strings.ToLower(strings.TrimSpace(p.ModuleName)),
			Tag:        strings.ToLower(strings.TrimSpace(p.Tag)),
		}
	}

	sort.Slice(plugins, func(i, j int) bool {
		return plugins[i].ModuleName < plugins[j].ModuleName
	})

	return normalizedBuildConfig{
		RRVersion: strings.ToLower(strings.TrimSpace(br.RRVersion)),
		OS:        strings.ToLower(strings.TrimSpace(br.TargetPlatform.OS)),
		Arch:      strings.ToLower(strings.TrimSpace(br.TargetPlatform.Arch)),
		Plugins:   plugins,
	}
}

// normalizedBuildConfig represents a normalized build configuration for cache key generation.
type normalizedBuildConfig struct {
	RRVersion string             `json:"rr_version"`
	OS        string             `json:"os"`
	Arch      string             `json:"arch"`
	Plugins   []normalizedPlugin `json:"plugins"`
}

// normalizedPlugin represents a normalized plugin for cache key generation.
type normalizedPlugin struct {
	ModuleName string `json:"module"`
	Tag        string `json:"tag"`
}

// BinaryFilename returns the expected filename for the built binary.
func (br *BuildRequest) BinaryFilename() string {
	filename := fmt.Sprintf("roadrunner-%s-%s", br.TargetPlatform.OS, br.TargetPlatform.Arch)

	if br.TargetPlatform.OS == "windows" {
		filename += ".exe"
	}

	return filename
}
