package engine

import (
	"time"

	"github.com/pfisterer/notifications/internal/model"
)

// Policy are the rules against flooding people with mail.
type Policy struct {
	// Location is the time zone of days (for the daily limit) and summary times.
	Location *time.Location
	// BundleCollect is how long an immediate notification waits for more of its type for the same person before the first mail goes out, so a burst becomes one mail.
	BundleCollect time.Duration
	// BundleWindow: after an immediate mail of a type, further ones of that type for the same person wait until this much time has passed and then go out together.
	BundleWindow time.Duration
	// DailyLimit is the most immediate mails a person gets per day; further ones go into the summary.
	DailyLimit int
	// ReminderAfter are the times after the first notification at which an open action is reminded of (at most this many reminders).
	ReminderAfter []time.Duration
	// DigestHour is the local hour summaries go out at (daily, or Mondays for weekly ones).
	DigestHour int
	// RetryBackoff are the waits after a failed send, by attempt; the last one repeats.
	RetryBackoff []time.Duration
	// MaxAttempts is how often a send is tried before the notification is given up.
	MaxAttempts int
	// Retention is how long delivered notifications, closed events and sent mails are kept.
	Retention time.Duration
}

// DefaultPolicy is the policy of the design: bundle within 15 minutes, at most 5 immediate mails a day, reminders after 2 and 7 days, summaries at 8 o'clock.
func DefaultPolicy() Policy {
	loc, err := time.LoadLocation("Europe/Berlin")
	if err != nil {
		loc = time.UTC
	}
	return Policy{
		Location:      loc,
		BundleCollect: 2 * time.Minute,
		BundleWindow:  15 * time.Minute,
		DailyLimit:    5,
		ReminderAfter: []time.Duration{48 * time.Hour, 7 * 24 * time.Hour},
		DigestHour:    8,
		RetryBackoff:  []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour},
		MaxAttempts:   10,
		Retention:     90 * 24 * time.Hour,
	}
}

// DayStart is the start of t's day in loc (the daily limit counts per calendar day).
func DayStart(t time.Time, loc *time.Location) time.Time {
	x := t.In(loc)
	return time.Date(x.Year(), x.Month(), x.Day(), 0, 0, 0, 0, loc)
}

// NextDigestSlot is the first summary time strictly after ref: daily at hour, or weekly on Monday at hour.
func NextDigestSlot(ref time.Time, d model.Digest, hour int, loc *time.Location) time.Time {
	x := ref.In(loc)
	slot := time.Date(x.Year(), x.Month(), x.Day(), hour, 0, 0, 0, loc)
	if !slot.After(x) {
		slot = time.Date(x.Year(), x.Month(), x.Day()+1, hour, 0, 0, 0, loc)
	}
	if d == model.DigestWeekly {
		for slot.Weekday() != time.Monday {
			slot = time.Date(slot.Year(), slot.Month(), slot.Day()+1, hour, 0, 0, 0, loc)
		}
	}
	return slot
}

func (p Policy) backoff(attempts int) time.Duration {
	if len(p.RetryBackoff) == 0 {
		return time.Minute
	}
	if attempts >= len(p.RetryBackoff) {
		return p.RetryBackoff[len(p.RetryBackoff)-1]
	}
	if attempts < 0 {
		attempts = 0
	}
	return p.RetryBackoff[attempts]
}
