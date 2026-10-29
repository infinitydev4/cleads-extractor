// Package metrics exposes Prometheus metrics. Throughput is split in four
// series on purpose — raw, unique, complete, CRM-useful — because raw volume
// alone hides duplicates and empty records.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

type Metrics struct {
	Registry *prometheus.Registry

	RecordsRaw       *prometheus.CounterVec
	RecordsUnique    *prometheus.CounterVec
	RecordsComplete  *prometheus.CounterVec
	RecordsUseful    *prometheus.CounterVec
	DedupCacheHits   prometheus.Counter
	RecordsUnrelated *prometheus.CounterVec

	SourceRequests *prometheus.CounterVec
	SourceLatency  *prometheus.HistogramVec
	SourceRate     *prometheus.GaugeVec
	CircuitOpen    *prometheus.GaugeVec

	TasksInFlight *prometheus.GaugeVec
	TasksPending  *prometheus.GaugeVec
	UpsertLatency prometheus.Histogram
	Enriched      *prometheus.CounterVec

	HTTPRequests *prometheus.CounterVec
	HTTPLatency  *prometheus.HistogramVec
}

func New() *Metrics {
	r := prometheus.NewRegistry()
	r.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := &Metrics{
		Registry: r,
		RecordsRaw: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cleads_records_raw_total", Help: "Records returned by sources, duplicates included.",
		}, []string{"source"}),
		RecordsUnique: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cleads_records_unique_total", Help: "Records newly attached to a job after deduplication.",
		}, []string{"source"}),
		RecordsComplete: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cleads_records_complete_total", Help: "Unique records meeting the completeness definition.",
		}, []string{"source"}),
		RecordsUseful: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cleads_records_crm_useful_total", Help: "Unique records usable in the CRM (contactable, operating).",
		}, []string{"source"}),
		RecordsUnrelated: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cleads_records_unrelated_total", Help: "Records dropped because they do not match the searched activity.",
		}, []string{"source"}),
		DedupCacheHits: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "cleads_dedup_cache_hits_total", Help: "Records dropped by the in-process dedup cache before reaching the database.",
		}),
		SourceRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cleads_source_requests_total", Help: "Source requests by outcome.",
		}, []string{"source", "outcome"}),
		SourceLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "cleads_source_request_seconds", Help: "Source request latency.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2, 5, 10, 30, 60, 300},
		}, []string{"source"}),
		SourceRate: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cleads_source_rate_limit", Help: "Current adaptive request rate per worker (req/s).",
		}, []string{"source"}),
		CircuitOpen: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cleads_source_circuit_open", Help: "1 when the source circuit breaker is open.",
		}, []string{"source"}),
		TasksInFlight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cleads_tasks_in_flight", Help: "Tasks being executed by this worker.",
		}, []string{"source"}),
		TasksPending: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "cleads_tasks_pending", Help: "Queue depth by source (cluster-wide).",
		}, []string{"source"}),
		UpsertLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "cleads_store_upsert_seconds", Help: "Batch upsert latency.",
			Buckets: prometheus.ExponentialBuckets(0.001, 2, 14),
		}),
		Enriched: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cleads_enrich_total", Help: "Website enrichments by outcome.",
		}, []string{"outcome"}),
		HTTPRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "cleads_http_requests_total", Help: "API requests.",
		}, []string{"route", "code"}),
		HTTPLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name: "cleads_http_request_seconds", Help: "API latency.",
			Buckets: []float64{.0005, .001, .0025, .005, .01, .025, .05, .1, .25, .5, 1, 5},
		}, []string{"route"}),
	}
	r.MustRegister(m.RecordsRaw, m.RecordsUnique, m.RecordsUnrelated, m.RecordsComplete, m.RecordsUseful, m.DedupCacheHits,
		m.SourceRequests, m.SourceLatency, m.SourceRate, m.CircuitOpen, m.TasksInFlight, m.TasksPending,
		m.UpsertLatency, m.Enriched, m.HTTPRequests, m.HTTPLatency)
	return m
}
