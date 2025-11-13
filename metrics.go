package veloxmiddleware

import (
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

var (
	metricsOnce sync.Once

	// Cache metrics
	cacheHits = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "velox",
		Name:      "cache_hits_total",
		Help:      "Total number of cache hits",
	})

	cacheMisses = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "velox",
		Name:      "cache_misses_total",
		Help:      "Total number of cache misses",
	})

	cacheEvictions = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "velox",
		Name:      "cache_evictions_total",
		Help:      "Total number of cache evictions",
	}, []string{"reason"}) // expired, lru

	cacheEntries = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "velox",
		Name:      "cache_entries_count",
		Help:      "Current number of cache entries",
	})

	cacheSize = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "velox",
		Name:      "cache_size_bytes",
		Help:      "Current cache size in bytes",
	})

	cacheAccessAge = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "velox",
		Name:      "cache_access_age_seconds",
		Help:      "Age of the most recently accessed cache entry",
	})

	cacheLookupDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "velox",
		Name:      "cache_lookup_duration_seconds",
		Help:      "Cache lookup duration",
		Buckets:   prometheus.DefBuckets,
	})

	cacheWriteDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "velox",
		Name:      "cache_write_duration_seconds",
		Help:      "Cache write duration",
		Buckets:   prometheus.DefBuckets,
	})

	// Build metrics
	buildsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "velox",
		Name:      "builds_total",
		Help:      "Total number of build requests",
	}, []string{"status", "cache_status"}) // status: success|error|timeout, cache_status: hit|miss

	forceRebuilds = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "velox",
		Name:      "force_rebuilds_total",
		Help:      "Total number of forced rebuilds",
	})

	activeBuilds = prometheus.NewGauge(prometheus.GaugeOpts{
		Namespace: "velox",
		Name:      "builds_active",
		Help:      "Number of currently active builds",
	})

	buildDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "velox",
		Name:      "build_duration_seconds",
		Help:      "Build duration (cache misses only)",
		Buckets:   []float64{5, 10, 30, 60, 120, 300, 600}, // 5s to 10m
	}, []string{"os", "arch"})

	binarySize = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "velox",
		Name:      "binary_size_bytes",
		Help:      "Binary size distribution",
		Buckets:   []float64{10485760, 52428800, 104857600, 209715200, 524288000}, // 10MB to 500MB
	}, []string{"os", "arch"})

	// Error metrics
	errorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "velox",
		Name:      "errors_total",
		Help:      "Total number of errors",
	}, []string{"type"}) // validation|velox_server|cache_write|timeout|stream

	retriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "velox",
		Name:      "retries_total",
		Help:      "Total number of retry attempts",
	}, []string{"reason"}) // timeout|connection|server_error

	// Streaming metrics
	streamTTFB = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "velox",
		Name:      "stream_ttfb_seconds",
		Help:      "Time to first byte",
		Buckets:   prometheus.DefBuckets,
	}, []string{"cache_status"}) // hit|miss

	clientDisconnects = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "velox",
		Name:      "client_disconnects_total",
		Help:      "Total number of client disconnects",
	}, []string{"stage"}) // queue|cache_read|build|stream
)

// initMetrics registers all Prometheus metrics.
func initMetrics() {
	metricsOnce.Do(func() {
		// Register all metrics explicitly
		// This is required even when MetricsCollector() is implemented
		prometheus.MustRegister(
			// Cache metrics
			cacheHits,
			cacheMisses,
			cacheEvictions,
			cacheEntries,
			cacheSize,
			cacheAccessAge,
			cacheLookupDuration,
			cacheWriteDuration,

			// Build metrics
			buildsTotal,
			forceRebuilds,
			activeBuilds,
			buildDuration,
			binarySize,

			// Error metrics
			errorsTotal,
			retriesTotal,

			// Streaming metrics
			streamTTFB,
			clientDisconnects,
		)
	})
}

// Metric helper functions

func metricsIncCacheHits() {
	cacheHits.Inc()
}

func metricsIncCacheMisses() {
	cacheMisses.Inc()
}

func metricsIncCacheEvictions(reason string) {
	cacheEvictions.WithLabelValues(reason).Inc()
}

func metricsSetCacheEntries(count int) {
	cacheEntries.Set(float64(count))
}

func metricsSetCacheSize(bytes int64) {
	cacheSize.Set(float64(bytes))
}

func metricsSetCacheAccessAge(age time.Duration) {
	cacheAccessAge.Set(age.Seconds())
}

func metricsIncBuildsByStatus(status, cacheStatus string) {
	buildsTotal.WithLabelValues(status, cacheStatus).Inc()
}

func metricsIncForceRebuilds() {
	forceRebuilds.Inc()
}

func metricsSetActiveBuilds(count int) {
	activeBuilds.Set(float64(count))
}

func metricsObserveBuildDuration(duration time.Duration, os, arch string) {
	buildDuration.WithLabelValues(os, arch).Observe(duration.Seconds())
}

func metricsObserveBinarySize(bytes int64, os, arch string) {
	binarySize.WithLabelValues(os, arch).Observe(float64(bytes))
}

func metricsIncErrors(errorType string) {
	errorsTotal.WithLabelValues(errorType).Inc()
}

func metricsIncRetries(reason string) {
	retriesTotal.WithLabelValues(reason).Inc()
}

func metricsObserveStreamTTFB(duration time.Duration, cacheStatus string) {
	streamTTFB.WithLabelValues(cacheStatus).Observe(duration.Seconds())
}

func metricsIncClientDisconnects(stage string) {
	clientDisconnects.WithLabelValues(stage).Inc()
}

// MetricsCollector returns all Prometheus collectors for this plugin.
// This method is called by RoadRunner's metrics plugin to collect metrics.
//
// IMPORTANT: Metrics must also be registered via prometheus.MustRegister() in initMetrics().
// The MetricsCollector() method alone is not sufficient - explicit registration is required.
func (p *Plugin) MetricsCollector() []prometheus.Collector {
	return []prometheus.Collector{
		// Cache metrics
		cacheHits,
		cacheMisses,
		cacheEvictions,
		cacheEntries,
		cacheSize,
		cacheAccessAge,
		cacheLookupDuration,
		cacheWriteDuration,

		// Build metrics
		buildsTotal,
		forceRebuilds,
		activeBuilds,
		buildDuration,
		binarySize,

		// Error metrics
		errorsTotal,
		retriesTotal,

		// Streaming metrics
		streamTTFB,
		clientDisconnects,
	}
}
