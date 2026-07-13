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
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	events   *prometheus.CounterVec
	suggest  prometheus.Histogram
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
	}
	reg.MustRegister(m.requests, m.duration, m.events, m.suggest)
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
