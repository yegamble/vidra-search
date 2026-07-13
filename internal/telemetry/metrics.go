package telemetry

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Metrics holds the vidra-search Prometheus instruments on a PRIVATE registry
// (never the process-global default) so tests build independent instances and no
// global state leaks. All label spaces are bounded by construction: the HTTP
// method, the Echo ROUTE TEMPLATE (never a raw URL/id/query), a status class,
// and a fixed set of event type/outcome values.
type Metrics struct {
	registry           *prometheus.Registry
	requests           *prometheus.CounterVec
	duration           *prometheus.HistogramVec
	events             *prometheus.CounterVec
	suggest            prometheus.Histogram
	eventLag           prometheus.Histogram
	rollupDuration     *prometheus.HistogramVec
	workerErrors       *prometheus.CounterVec
	trendingRejections *prometheus.CounterVec
	reconcileAge       prometheus.Gauge
	modelLoadErrors    prometheus.Counter
	loadedModel        *prometheus.GaugeVec
	shadowEval         *prometheus.GaugeVec
}

// NewMetrics builds the instruments on a fresh private registry, together with
// the standard Go-runtime and process collectors.
func NewMetrics() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		registry: reg,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vidra_search_http_requests_total",
			Help: "Total HTTP requests, labelled by method, route template, and status class.",
		}, []string{"method", "route", "status_class"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vidra_search_http_request_duration_seconds",
			Help:    "HTTP request duration in seconds, labelled by method, route template, and status class.",
			Buckets: prometheus.DefBuckets,
		}, []string{"method", "route", "status_class"}),
		events: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vidra_search_events_total",
			Help: "Total ingested events by domain/behavioral type and outcome (accepted|duplicate|failed|ignored|counted).",
		}, []string{"type", "outcome"}),
		suggest: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "vidra_search_suggest_duration_seconds",
			Help:    "Suggestion pipeline duration in seconds (normalize → candidates → blend).",
			Buckets: []float64{.001, .0025, .005, .01, .025, .05, .1, .25, .5, 1},
		}),
		eventLag: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "vidra_search_event_lag_seconds",
			Help:    "Delay between an event's occurred_at and its processing at intake.",
			Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300},
		}),
		rollupDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vidra_search_rollup_duration_seconds",
			Help:    "Background worker pass duration in seconds, labelled by worker.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}, []string{"worker"}),
		workerErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vidra_search_worker_errors_total",
			Help: "Total background worker pass failures, labelled by worker.",
		}, []string{"worker"}),
		trendingRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vidra_search_trending_gate_rejections_total",
			Help: "Trending candidates rejected by a gate, labelled by domain and reason.",
		}, []string{"domain", "reason"}),
		reconcileAge: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "vidra_search_reconcile_age_seconds",
			Help: "Seconds since the last reconcile.end was received (-1 if none on record).",
		}),
		modelLoadErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "vidra_search_model_load_errors_total",
			Help: "Total learned-model artifact load failures (missing/corrupt/malformed); the service keeps the previous model or heuristic.",
		}),
		loadedModel: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vidra_search_loaded_model",
			Help: "The currently-served model per kind (value 1 on the active version label).",
		}, []string{"kind", "version"}),
		shadowEval: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vidra_search_shadow_eval",
			Help: "Shadow-evaluation metric per model version and metric name (ndcg@10, mrr@10, vs_production, vs_heuristic).",
		}, []string{"version", "metric"}),
	}
	reg.MustRegister(m.requests, m.duration, m.events, m.suggest,
		m.eventLag, m.rollupDuration, m.workerErrors, m.trendingRejections, m.reconcileAge,
		m.modelLoadErrors, m.loadedModel, m.shadowEval)
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// ObserveRequest records one completed HTTP request. route MUST be the Echo
// route template so cardinality stays bounded; an empty route folds to
// "unmatched".
func (m *Metrics) ObserveRequest(method, route string, status int, d time.Duration) {
	if route == "" {
		route = "unmatched"
	}
	class := statusClass(status)
	m.requests.WithLabelValues(method, route, class).Inc()
	m.duration.WithLabelValues(method, route, class).Observe(d.Seconds())
}

// ObserveEvent records the outcome of processing one event. typ is bounded to
// the known type set by the caller (unknown types collapse to "unknown").
func (m *Metrics) ObserveEvent(typ, outcome string) {
	m.events.WithLabelValues(typ, outcome).Inc()
}

// ObserveSuggest records one suggestion pipeline duration.
func (m *Metrics) ObserveSuggest(d time.Duration) {
	m.suggest.Observe(d.Seconds())
}

// ObserveEventLag records the occurred_at→processed delay for one event.
func (m *Metrics) ObserveEventLag(seconds float64) {
	if seconds < 0 {
		return
	}
	m.eventLag.Observe(seconds)
}

// ObserveRollup records one background worker pass duration.
func (m *Metrics) ObserveRollup(worker string, seconds float64) {
	m.rollupDuration.WithLabelValues(worker).Observe(seconds)
}

// IncWorkerError counts one worker pass failure.
func (m *Metrics) IncWorkerError(worker string) {
	m.workerErrors.WithLabelValues(worker).Inc()
}

// IncTrendingRejection counts one gate-rejected trending candidate.
func (m *Metrics) IncTrendingRejection(domain, reason string) {
	m.trendingRejections.WithLabelValues(domain, reason).Inc()
}

// SetReconcileAge records the age of the last reconcile.end.
func (m *Metrics) SetReconcileAge(seconds float64) {
	m.reconcileAge.Set(seconds)
}

// IncModelLoadError counts one learned-model artifact load failure.
func (m *Metrics) IncModelLoadError() {
	m.modelLoadErrors.Inc()
}

// SetLoadedModel marks version as the served model for kind (resetting the gauge
// so exactly one version label reads 1 per kind).
func (m *Metrics) SetLoadedModel(kind, version string) {
	m.loadedModel.DeletePartialMatch(prometheus.Labels{"kind": kind})
	m.loadedModel.WithLabelValues(kind, version).Set(1)
}

// SetShadowMetric records one shadow-evaluation metric for a model version.
func (m *Metrics) SetShadowMetric(version, metric string, value float64) {
	m.shadowEval.WithLabelValues(version, metric).Set(value)
}

// TableDepth is one table's approximate row count.
type TableDepth struct {
	Table string
	Rows  int64
}

// RegisterTableDepthSource installs vidra_search_table_rows{table} pulled from
// source at scrape time (no background goroutine). A source error is swallowed so
// a transient DB hiccup never fails the scrape. Call at most once.
func (m *Metrics) RegisterTableDepthSource(source func(context.Context) ([]TableDepth, error)) {
	m.registry.MustRegister(&tableDepthCollector{
		desc: prometheus.NewDesc(
			"vidra_search_table_rows",
			"Approximate row count per search-schema table (planner statistics).",
			[]string{"table"}, nil,
		),
		source: source,
	})
}

type tableDepthCollector struct {
	desc   *prometheus.Desc
	source func(context.Context) ([]TableDepth, error)
}

func (c *tableDepthCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *tableDepthCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.source(ctx)
	if err != nil {
		return
	}
	for _, r := range rows {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue, float64(r.Rows), r.Table)
	}
}

// DocCount is one document-count sample partitioned by eligibility.
type DocCount struct {
	Eligible bool
	Count    int64
}

// RegisterDocumentGaugeSource installs vidra_search_documents{eligible} whose
// values are pulled from source at scrape time (no background goroutine). A
// source error is swallowed so a transient DB hiccup never fails the scrape.
// Call at most once.
func (m *Metrics) RegisterDocumentGaugeSource(source func(context.Context) ([]DocCount, error)) {
	m.registry.MustRegister(&documentGaugeCollector{
		desc: prometheus.NewDesc(
			"vidra_search_documents",
			"Number of indexed documents by static eligibility.",
			[]string{"eligible"}, nil,
		),
		source: source,
	})
}

// Handler returns the Prometheus scrape handler for this registry.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

type documentGaugeCollector struct {
	desc   *prometheus.Desc
	source func(context.Context) ([]DocCount, error)
}

func (c *documentGaugeCollector) Describe(ch chan<- *prometheus.Desc) { ch <- c.desc }

func (c *documentGaugeCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := c.source(ctx)
	if err != nil {
		return // keep the scrape healthy on a transient source error
	}
	for _, r := range rows {
		ch <- prometheus.MustNewConstMetric(c.desc, prometheus.GaugeValue,
			float64(r.Count), strconv.FormatBool(r.Eligible))
	}
}

// statusClass buckets an HTTP status code into a bounded label value.
func statusClass(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return "other"
	}
}
