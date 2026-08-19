package gateway

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"time"
)

type metrics struct {
	requests         atomic.Uint64
	completed        atomic.Uint64
	failed           atomic.Uint64
	active           atomic.Int64
	upstreamRequests atomic.Uint64
	upstreamErrors   atomic.Uint64
	queueWaits       atomic.Uint64
	queueRejected    atomic.Uint64
	durationNanos    atomic.Uint64
}

func (m *metrics) writePrometheus(w http.ResponseWriter, queueDepth int) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	f := func(name string, value any) { _, _ = fmt.Fprintf(w, "shepard_%s %v\n", name, value) }
	f("requests_total", m.requests.Load())
	f("requests_completed_total", m.completed.Load())
	f("requests_failed_total", m.failed.Load())
	f("requests_active", m.active.Load())
	f("upstream_requests_total", m.upstreamRequests.Load())
	f("upstream_errors_total", m.upstreamErrors.Load())
	f("queue_waits_total", m.queueWaits.Load())
	f("queue_rejected_total", m.queueRejected.Load())
	f("queue_depth", queueDepth)
	f("request_duration_seconds_total", float64(m.durationNanos.Load())/float64(time.Second))
}
