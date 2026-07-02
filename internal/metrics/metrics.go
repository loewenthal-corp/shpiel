// Package metrics defines Shpiel's Prometheus instrumentation. All metrics
// hang off an explicit registry so tests can create isolated instances.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Sources for download byte accounting.
const (
	SourceCache    = "cache"
	SourceUpstream = "upstream"
)

// Metrics holds all Shpiel instruments.
type Metrics struct {
	Registry *prometheus.Registry

	HTTPRequests       *prometheus.CounterVec   // handler, method, code
	HTTPDuration       *prometheus.HistogramVec // handler, method
	DownloadBytes      *prometheus.CounterVec   // source
	UploadBytes        *prometheus.CounterVec   // backend
	PullThroughFetches *prometheus.CounterVec   // kind, outcome
	Commits            *prometheus.CounterVec   // outcome
	InflightRequests   prometheus.Gauge

	ReplicationQueueDepth prometheus.Gauge
	ReplicationJobs       *prometheus.CounterVec // target, outcome
}

// New creates a Metrics with its own registry, including the standard Go
// and process collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "shpiel_http_requests_total",
			Help: "HTTP requests served, by handler, method, and status code.",
		}, []string{"handler", "method", "code"}),
		HTTPDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "shpiel_http_request_duration_seconds",
			Help:    "HTTP request latency, by handler and method.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2.5, 14), // 1ms .. ~6m
		}, []string{"handler", "method"}),
		DownloadBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "shpiel_download_bytes_total",
			Help: "Bytes served to clients, by source (cache or upstream).",
		}, []string{"source"}),
		UploadBytes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "shpiel_upload_bytes_total",
			Help: "Bytes written to backends, by backend.",
		}, []string{"backend"}),
		PullThroughFetches: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "shpiel_pullthrough_fetches_total",
			Help: "Pull-through fetches from upstream, by kind (manifest or blob) and outcome.",
		}, []string{"kind", "outcome"}),
		Commits: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "shpiel_commits_total",
			Help: "Commits accepted through the write path, by outcome.",
		}, []string{"outcome"}),
		InflightRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "shpiel_http_inflight_requests",
			Help: "HTTP requests currently being served.",
		}),
		ReplicationQueueDepth: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "shpiel_replication_queue_depth",
			Help: "Replication jobs waiting or retrying.",
		}),
		ReplicationJobs: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "shpiel_replication_jobs_total",
			Help: "Replication job executions, by target backend and outcome.",
		}, []string{"target", "outcome"}),
	}
	reg.MustRegister(
		m.HTTPRequests, m.HTTPDuration, m.DownloadBytes, m.UploadBytes,
		m.PullThroughFetches, m.Commits, m.InflightRequests,
		m.ReplicationQueueDepth, m.ReplicationJobs,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}
