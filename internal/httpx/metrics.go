package httpx

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"animind-backend-go/internal/cache"
	"animind-backend-go/internal/downloads"
	"animind-backend-go/internal/repository"
	"animind-backend-go/internal/syncplay"
)

// The Prometheus listener: what the Grafana dashboard reads.
//
// It answers on its own port rather than under /api so it never shares a route table with the
// public API. It is unauthenticated, which is only safe because nothing reaches it from
// outside: nginx does not proxy it and the compose stack does not publish it.

const (
	metricsNamespace = "animind"
	// unmatchedRoute collapses requests that hit no route. Without it a path scan would
	// mint a fresh label value per probe and turn the metrics endpoint into the outage.
	unmatchedRoute = "unmatched"
)

// metrics is the collector set one process exports.
type metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
	// inflight counts handlers currently running, which is how a stuck media stream shows up
	// before it shows up as latency.
	inflight *prometheus.GaugeVec
	// downloads collectors (spec §7.10). The gauge is set by the manager's progress
	// ticker, the counters by its progress and failure paths; the histogram is
	// observed by the search handler around each indexer call.
	downloadsActive prometheus.Gauge
	downloadsBytes  prometheus.Counter
	downloadsFailed prometheus.Counter
	searchSeconds   prometheus.Histogram
}

func newMetrics(store cache.Store, hub *syncplay.Hub, db *repository.Store, fallback cache.FallbackCounter, jobs *downloads.Manager) *metrics {
	m := &metrics{
		registry: prometheus.NewRegistry(),
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: metricsNamespace, Subsystem: "http", Name: "requests_total",
			Help: "Requests handled, by route pattern, method and status class.",
		}, []string{"route", "method", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: metricsNamespace, Subsystem: "http", Name: "request_duration_seconds",
			Help:    "Time from handler entry to response finish.",
			Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30},
		}, []string{"route", "method"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: metricsNamespace, Subsystem: "http", Name: "requests_in_flight",
			Help: "Handlers currently running, by method.",
		}, []string{"method"}),
		downloadsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: metricsNamespace, Subsystem: "downloads", Name: "active",
			Help: "Download jobs not in a terminal state, across every account.",
		}),
		downloadsBytes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace, Subsystem: "downloads", Name: "bytes_total",
			Help: "Bytes downloaded into completed pieces, across all jobs since process start.",
		}),
		downloadsFailed: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace, Subsystem: "downloads", Name: "failed_total",
			Help: "Jobs that transitioned to failed, by any failure code.",
		}),
		searchSeconds: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name: "torrent_search_seconds",
			Help: "End-to-end duration of one /api/torrents/search call, including indexer fan-out.",
			// A cache hit answers in microseconds, a cold indexer fan-out in
			// seconds; the buckets must hold both ends without flattening them.
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}),
	}

	m.registry.MustRegister(
		m.requests, m.duration, m.inflight, m.searchSeconds,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	if jobs != nil {
		// The manager publishes into the same registry: active is set from
		// CountActiveDownloadJobs(nil) on each progress tick, bytes_total and
		// failed_total increment on the manager's own transitions.
		m.registry.MustRegister(m.downloadsActive, m.downloadsBytes, m.downloadsFailed)
		jobs.SetObserver(m)
	}
	if store != nil {
		m.registry.MustRegister(&cacheCollector{store: store, fallback: fallback})
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: metricsNamespace, Name: "cache_shared",
			Help: "One when shared state crosses processes, zero when it lives in this one.",
		}, func() float64 {
			if store.Shared() {
				return 1
			}
			return 0
		}))
	}
	if hub != nil {
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: metricsNamespace, Subsystem: "watchparty", Name: "rooms",
			Help: "Watch-party rooms held in memory.",
		}, func() float64 { return float64(hub.RoomCount()) }))
		m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Namespace: metricsNamespace, Subsystem: "watchparty", Name: "peers",
			Help: "Sockets currently attached to the hub.",
		}, func() float64 { return float64(hub.PeerCount()) }))
	}
	if db != nil {
		m.registry.MustRegister(newLibraryCollector(db))
		for _, stat := range []struct {
			name string
			help string
			read func(repository.PoolStats) int32
		}{
			{"connections_total", "Pooled database connections.", func(s repository.PoolStats) int32 { return s.Total }},
			{"connections_active", "Database connections serving a query now.", func(s repository.PoolStats) int32 { return s.Active }},
			{"connections_idle", "Idle database connections ready to serve.", func(s repository.PoolStats) int32 { return s.Idle }},
			{"connections_max", "Configured upper bound on the pool.", func(s repository.PoolStats) int32 { return s.Max }},
		} {
			// Captured per stat so the exported function reads its own field.
			read := stat.read
			m.registry.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
				Namespace: metricsNamespace, Subsystem: "db", Name: stat.name, Help: stat.help,
			}, func() float64 { return float64(read(db.PoolStats())) }))
		}
	}
	return m
}

// The downloads.Observer half of metrics: the manager calls these from its drive
// loops, so they must stay allocation-free and never block on the store.

// ActiveJobs records the manager's CountActiveDownloadJobs(nil) snapshot.
func (m *metrics) ActiveJobs(n int) { m.downloadsActive.Set(float64(n)) }

// BytesDownloaded accounts the bytes completed since the previous progress tick.
func (m *metrics) BytesDownloaded(delta int64) { m.downloadsBytes.Add(float64(delta)) }

// JobFailed accounts one transition into failed.
func (m *metrics) JobFailed() { m.downloadsFailed.Inc() }

var _ downloads.Observer = (*metrics)(nil)

// cacheCollector exports the store's outcome counters as proper monotonic counters, which is
// what lets a dashboard graph a hit ratio with rate() instead of guessing from gauges.
type cacheCollector struct {
	store cache.Store
	// fallback is nil when counting never leaves this process.
	fallback cache.FallbackCounter
}

var (
	cacheOpsDesc = prometheus.NewDesc(metricsNamespace+"_cache_operations_total",
		"Cache operations by outcome.", []string{"result", "backend"}, nil)
	rateFallbackDesc = prometheus.NewDesc(metricsNamespace+"_rate_limit_fallbacks_total",
		"Rate-limit verdicts taken from the in-process window because the shared store did not answer.",
		nil, nil)
)

func (c *cacheCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- cacheOpsDesc
	ch <- rateFallbackDesc
}

func (c *cacheCollector) Collect(ch chan<- prometheus.Metric) {
	stats := c.store.Stats()
	backend := c.store.Backend()
	for _, entry := range []struct {
		result string
		value  uint64
	}{
		{"hit", stats.Hits}, {"miss", stats.Misses}, {"set", stats.Writes},
		{"evict", stats.Evictions}, {"error", stats.Errors},
		{"allow", stats.Allowed}, {"deny", stats.Denied}, {"fallback", stats.Fallbacks},
	} {
		ch <- prometheus.MustNewConstMetric(cacheOpsDesc, prometheus.CounterValue,
			float64(entry.value), entry.result, backend)
	}
	// The fallback counter lives on the limiter, not the store: it counts verdicts, which is
	// the thing an operator needs to see climb during a Redis incident.
	if c.fallback != nil {
		ch <- prometheus.MustNewConstMetric(rateFallbackDesc, prometheus.CounterValue,
			float64(c.fallback.Fallbacks()))
	}
}

// observeRequests labels each request with the matched route pattern.
//
// Reading the pattern after the handler runs is what makes this work: chi fills it in while
// routing, so a middleware that sampled it on entry would label everything "unmatched".
//
// For the watch-party socket the recorded duration is the upgrade, not the connection's life:
// the hub hijacks and returns. The gauge and histogram are therefore about request handling,
// and `animind_watchparty_peers` is the one to watch for long-lived sockets.
func (s *Server) observeRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.metrics == nil {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		s.metrics.inflight.WithLabelValues(r.Method).Inc()
		defer s.metrics.inflight.WithLabelValues(r.Method).Dec()

		next.ServeHTTP(recorder, r)

		route := unmatchedRoute
		if pattern := chi.RouteContext(r.Context()).RoutePattern(); pattern != "" {
			route = pattern
		}
		seconds := time.Since(start).Seconds()
		s.metrics.duration.WithLabelValues(route, r.Method).Observe(seconds)
		// The exact code, not a class: a dashboard that lumps 401 and 429 together cannot
		// tell a broken client from a limiter doing its job.
		s.metrics.requests.WithLabelValues(route, r.Method, strconv.Itoa(recorder.status)).Inc()
	})
}

// metricsHandler serves the exposition on its own listener.
func (s *Server) metricsHandler() http.Handler {
	if s.metrics == nil {
		return http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	path := s.cfg.Metrics.Path
	if path == "" {
		path = "/metrics"
	}
	mux.Handle(path, promhttp.HandlerFor(s.metrics.registry, promhttp.HandlerOpts{}))
	// A scrape target that cannot say it is alive is a debugging dead end.
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
}

// libraryCollector exports the size of the catalog. COUNT queries are cheap on a personal
// library and expensive on a big one, so the answer is held for a minute and a scrape does
// not necessarily go to Postgres.
type libraryCollector struct {
	db      *repository.Store
	stale   time.Duration
	mu      sync.Mutex
	at      time.Time
	counts  repository.LibraryStats
	failups prometheus.Counter
}

func newLibraryCollector(db *repository.Store) *libraryCollector {
	return &libraryCollector{
		db:    db,
		stale: time.Minute,
		failups: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: metricsNamespace, Subsystem: "library", Name: "collection_failures_total",
			Help: "Times the catalog counts could not be read during a scrape.",
		}),
	}
}

var (
	libraryDesc = prometheus.NewDesc(metricsNamespace+"_library_entries",
		"Rows in the catalog, refreshed at most once a minute.", []string{"kind"}, nil)
)

func (c *libraryCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- libraryDesc
	ch <- c.failups.Desc()
}

func (c *libraryCollector) Collect(ch chan<- prometheus.Metric) {
	c.mu.Lock()
	if time.Since(c.at) > c.stale {
		// Bounded: a scrape must not hang on a database that has stopped answering, and this
		// lock is held while the query runs.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		counts, err := c.db.CountLibrary(ctx)
		cancel()
		if err != nil {
			c.failups.Inc()
		} else {
			c.counts = counts
			c.at = time.Now()
		}
	}
	counts := c.counts
	c.mu.Unlock()

	ch <- prometheus.MustNewConstMetric(libraryDesc, prometheus.GaugeValue, float64(counts.Shows), "shows")
	ch <- prometheus.MustNewConstMetric(libraryDesc, prometheus.GaugeValue, float64(counts.Episodes), "episodes")
	ch <- prometheus.MustNewConstMetric(libraryDesc, prometheus.GaugeValue, float64(counts.Users), "users")
	ch <- c.failups
}
