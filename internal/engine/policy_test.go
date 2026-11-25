package engine

import (
	"testing"
	"time"

	"github.com/pfisterer/notifications/internal/model"
)

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
