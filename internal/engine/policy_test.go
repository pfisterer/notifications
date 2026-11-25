package engine

import (
	"testing"
	"time"

	"github.com/pfisterer/notifications/internal/model"
)

func TestQuietUntil(t *testing.T) {
	q := model.DefaultPreferences().QuietHours
	cases := []struct {
		name  string
		t     time.Time
		quiet bool
		until time.Time
	}{
		{"weekday daytime", at(25, 10, 0), false, time.Time{}},
		{"evening", at(25, 20, 0), true, at(26, 7, 0)},
		{"after midnight", at(26, 3, 0), true, at(26, 7, 0)},
		{"end of night", at(26, 7, 0), false, time.Time{}},
		{"friday night runs into the weekend", at(27, 21, 0), true, at(30, 7, 0)},
		{"saturday", at(28, 12, 0), true, at(30, 7, 0)},
		{"sunday evening", at(29, 22, 0), true, at(30, 7, 0)},
	}
	for _, c := range cases {
		quiet, until := QuietUntil(c.t, q, berlin)
		if quiet != c.quiet || (quiet && !until.Equal(c.until)) {
			t.Errorf("%s: got %v until %v, want %v until %v", c.name, quiet, until, c.quiet, c.until)
		}
	}
	// A night inside one day and no weekends.
	day := model.QuietHours{Enabled: true, Start: "12:00", End: "13:00"}
	if quiet, until := QuietUntil(at(28, 12, 30), day, berlin); !quiet || !until.Equal(at(28, 13, 0)) {
		t.Errorf("midday: %v %v", quiet, until)
	}
	if quiet, _ := QuietUntil(at(25, 23, 0), model.QuietHours{Enabled: false, Start: "20:00", End: "07:00"}, berlin); quiet {
		t.Error("disabled quiet hours")
	}
}

func TestNextDigestSlot(t *testing.T) {
	if got := NextDigestSlot(at(25, 10, 0), model.DigestDaily, 8, berlin); !got.Equal(at(26, 8, 0)) {
		t.Errorf("daily: %v", got)
	}
	if got := NextDigestSlot(at(25, 7, 0), model.DigestDaily, 8, berlin); !got.Equal(at(25, 8, 0)) {
		t.Errorf("daily before the hour: %v", got)
	}
	if got := NextDigestSlot(at(25, 10, 0), model.DigestWeekly, 8, berlin); !got.Equal(at(30, 8, 0)) {
		t.Errorf("weekly: %v", got)
	}
	if got := NextDigestSlot(at(30, 8, 0), model.DigestWeekly, 8, berlin); !got.Equal(time.Date(2026, 12, 7, 8, 0, 0, 0, berlin)) {
		t.Errorf("weekly, at the slot: %v", got)
	}
}

func TestDayStart(t *testing.T) {
	if got := DayStart(at(25, 0, 30).UTC(), berlin); !got.Equal(at(25, 0, 0)) {
		t.Errorf("day start: %v", got)
	}
}
