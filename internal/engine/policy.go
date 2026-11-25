package engine

import (
	"time"

	"github.com/pfisterer/notifications/internal/model"
)

// Policy are the rules against flooding people with mail.
type Policy struct {
	// Location is the time zone of quiet hours, days (for the daily limit) and summary times.
	Location *time.Location
	// BundleCollect is how long an immediate notification waits for more of its type for the same person before the first mail goes out, so a burst becomes one mail.
	BundleCollect time.Duration
	// BundleWindow: after an immediate mail of a type, further ones of that type for the same person wait until this much time has passed and then go out together.
	BundleWindow time.Duration
	// DailyLimit is the most immediate mails a person gets per day; further ones go into the summary.
	DailyLimit int
	// UrgentWithin: an action due within this time is delivered during quiet hours too.
	UrgentWithin time.Duration
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

// DefaultPolicy is the policy of the design: bundle within 15 minutes, at most 5 immediate mails a day, reminders after 2 and 7 days, urgent within 48 hours, summaries at 8 o'clock.
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
		UrgentWithin:  48 * time.Hour,
		ReminderAfter: []time.Duration{48 * time.Hour, 7 * 24 * time.Hour},
		DigestHour:    8,
		RetryBackoff:  []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute, time.Hour},
		MaxAttempts:   10,
		Retention:     90 * 24 * time.Hour,
	}
}

// QuietUntil says whether t lies in the quiet hours q and, if so, when they end. Nights and weekends that touch are one stretch: Friday 21:00 is quiet until Monday at the end of the night.
func QuietUntil(t time.Time, q model.QuietHours, loc *time.Location) (bool, time.Time) {
	if !q.Enabled {
		return false, time.Time{}
	}
	x := t.In(loc)
	quiet := false
	// Each step leaves one stretch (a night or a weekend); a handful is enough for any combination.
	for range 8 {
		in, end := quietStretch(x, q, loc)
		if !in {
			break
		}
		quiet = true
		x = end
	}
	return quiet, x
}

// quietStretch says whether x lies in a night or a weekend and when that one ends.
func quietStretch(x time.Time, q model.QuietHours, loc *time.Location) (bool, time.Time) {
	y, mo, d := x.Date()
	if q.Weekends && (x.Weekday() == time.Saturday || x.Weekday() == time.Sunday) {
		days := 1
		if x.Weekday() == time.Saturday {
			days = 2
		}
		return true, time.Date(y, mo, d+days, 0, 0, 0, 0, loc)
	}
	s, err1 := model.ParseClock(q.Start)
	e, err2 := model.ParseClock(q.End)
	if err1 != nil || err2 != nil || s == e {
		return false, time.Time{}
	}
	m := x.Hour()*60 + x.Minute()
	at := func(day, minutes int) time.Time { return time.Date(y, mo, d+day, minutes/60, minutes%60, 0, 0, loc) }
	switch {
	case s < e && m >= s && m < e:
		return true, at(0, e)
	case s > e && m >= s:
		return true, at(1, e)
	case s > e && m < e:
		return true, at(0, e)
	}
	return false, time.Time{}
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

// urgent says whether a notification due at due must not wait for quiet hours to end: the deadline lies ahead, within UrgentWithin. One that has passed waits like any other.
func (p Policy) urgent(due *time.Time, now time.Time) bool {
	return due != nil && due.After(now) && due.Sub(now) <= p.UrgentWithin
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
