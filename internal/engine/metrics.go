package engine

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/promtext"
	"github.com/pfisterer/notifications/internal/store"
)

// Metrics are the service's counters, kept in memory (a restart sets them to zero, which rate() treats as a reset).
type Metrics struct {
	Received   *promtext.CounterVec
	Delivered  *promtext.CounterVec
	Messages   *promtext.CounterVec
	Bundled    *promtext.CounterVec
	Suppressed *promtext.CounterVec
	Deferred   *promtext.CounterVec
	Failed     *promtext.CounterVec
	Panics     *promtext.CounterVec
	lastPass   atomic.Int64
}

// NewMetrics makes the counters, with the series that alerts look at already at zero.
func NewMetrics() *Metrics {
	m := &Metrics{
		Received:   promtext.NewCounterVec("notify_events_received_total", "Events accepted, by sending service token and class (duplicates and resolves not counted).", "sender", "class"),
		Delivered:  promtext.NewCounterVec("notify_delivered_total", "Notifications delivered, by channel and kind (immediate or digest).", "channel", "kind"),
		Messages:   promtext.NewCounterVec("notify_messages_total", "Messages (mails) sent, by channel and kind.", "channel", "kind"),
		Bundled:    promtext.NewCounterVec("notify_bundled_total", "Notifications that went out in one immediate message together with others of their type (n-1 per message).", "channel"),
		Suppressed: promtext.NewCounterVec("notify_suppressed_total", "Notifications or events not delivered on purpose, by reason (duplicate, own_action, resolved, unsubscribed, off, no_recipient).", "reason"),
		Deferred:   promtext.NewCounterVec("notify_deferred_total", "Immediate notifications held back or moved to the summary, by reason (quiet_hours, daily_limit).", "reason"),
		Failed:     promtext.NewCounterVec("notify_failed_total", "Failures, by stage (send: one attempt; gave_up: a notification given up after every attempt; resolve: a group could not be resolved).", "stage"),
		Panics:     promtext.NewCounterVec("notify_loop_panics_total", "Recovered panics of the delivery loop.", "loop"),
	}
	m.Delivered.Init("email", "immediate")
	m.Delivered.Init("email", "digest")
	m.Messages.Init("email", "immediate")
	m.Messages.Init("email", "digest")
	m.Bundled.Init("email")
	for _, r := range []string{"duplicate", "own_action", "resolved", "unsubscribed", "off"} {
		m.Suppressed.Init(r)
	}
	m.Deferred.Init("quiet_hours")
	m.Deferred.Init("daily_limit")
	for _, s := range []string{"send", "gave_up", "resolve"} {
		m.Failed.Init(s)
	}
	m.Panics.Init("dispatch")
	return m
}

// Render writes the counters and the gauges read from the database.
func (m *Metrics) Render(ctx context.Context, s *store.Store) ([]byte, error) {
	var w promtext.Writer
	for _, c := range []*promtext.CounterVec{m.Received, m.Delivered, m.Messages, m.Bundled, m.Suppressed, m.Deferred, m.Failed, m.Panics} {
		c.WriteTo(&w)
	}
	counts, err := s.CountByStatus(ctx)
	if err != nil {
		return nil, err
	}
	w.Header("notify_waiting", "gauge", "Notifications waiting: pending (for an immediate message) and digest (for the next summary).")
	w.Sample("notify_waiting", float64(counts[store.StatusPending]), "status", store.StatusPending)
	w.Sample("notify_waiting", float64(counts[store.StatusDigest]), "status", store.StatusDigest)
	w.Header("notify_last_pass_timestamp_seconds", "gauge", "When the delivery loop last finished a pass (Unix time).")
	w.Sample("notify_last_pass_timestamp_seconds", float64(m.lastPass.Load()))
	return w.Bytes(), nil
}

func (m *Metrics) passDone(t time.Time) { m.lastPass.Store(t.Unix()) }

func (m *Metrics) add(c *promtext.CounterVec, n int, labels ...string) {
	for range n {
		c.Inc(labels...)
	}
}
