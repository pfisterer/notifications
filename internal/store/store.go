// Package store keeps events, the notifications fanned out of them, the mails sent and people's preferences, in PostgreSQL (SQLite in the tests) through gorm.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/pfisterer/notifications/internal/model"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Status of a notification.
const (
	// StatusPending waits for immediate delivery (bundling, quiet hours, a retry).
	StatusPending = "pending"
	// StatusDigest waits for the person's next summary.
	StatusDigest = "digest"
	// StatusSent went out in an immediate mail.
	StatusSent = "sent"
	// StatusDigested went out in a summary.
	StatusDigested = "digested"
	// StatusSuppressed was dropped; Reason says why.
	StatusSuppressed = "suppressed"
	// StatusFailed could not be delivered after every attempt.
	StatusFailed = "failed"
)

// Event is one event as received. Recipients are kept as sent until the fan-out has resolved them into notifications.
type Event struct {
	ID uint64 `gorm:"primaryKey"`
	// Sender is the name of the service token the event came with; duplicates are recognised per sender.
	Sender     string `gorm:"size:100;not null"`
	Source     string `gorm:"size:100;not null"`
	Type       string `gorm:"size:100;not null"`
	Key        string `gorm:"size:300;not null"`
	Class      string `gorm:"size:20;not null"`
	Actor      string `gorm:"size:320"`
	Due        *time.Time
	Object     string    `gorm:"type:text"`
	Data       string    `gorm:"type:text"`
	Recipients string    `gorm:"type:text"`
	ReceivedAt time.Time `gorm:"not null;index"`
	// FannedOut is set once the recipients have become notifications.
	FannedOut  bool `gorm:"not null;default:false;index"`
	ResolvedAt *time.Time
}

// ObjectValue decodes Object.
func (e *Event) ObjectValue() model.Object {
	var o model.Object
	_ = json.Unmarshal([]byte(e.Object), &o)
	return o
}

// DataValue decodes Data.
func (e *Event) DataValue() map[string]any {
	d := map[string]any{}
	_ = json.Unmarshal([]byte(e.Data), &d)
	return d
}

// RecipientsValue decodes Recipients.
func (e *Event) RecipientsValue() []model.Recipient {
	var r []model.Recipient
	_ = json.Unmarshal([]byte(e.Recipients), &r)
	return r
}

// Notification is one event for one person, or a reminder of it.
type Notification struct {
	ID        uint64 `gorm:"primaryKey"`
	EventID   uint64 `gorm:"not null;index"`
	Event     *Event `gorm:"foreignKey:EventID;constraint:OnDelete:CASCADE"`
	Recipient string `gorm:"size:320;not null;index:idx_notif_recipient_status,priority:1"`
	Type      string `gorm:"size:100;not null"`
	Class     string `gorm:"size:20;not null"`
	// Reminder is 0 for the notification itself, 1 and 2 for the reminders of it.
	Reminder int    `gorm:"not null;default:0"`
	Status   string `gorm:"size:20;not null;index:idx_notif_recipient_status,priority:2;index"`
	// Reason says why a notification was suppressed or went to the summary instead (quiet_hours, daily_limit, preference, unsubscribed, off, resolved, own_action, ...).
	Reason    string    `gorm:"size:40"`
	CreatedAt time.Time `gorm:"not null;index"`
	// NotBefore holds a pending notification back (quiet hours, a retry after a failed send).
	NotBefore *time.Time
	SentAt    *time.Time
	MailID    *uint64
	// RemindersQueued counts the reminders created from this notification.
	RemindersQueued int `gorm:"not null;default:0"`
	Attempts        int `gorm:"not null;default:0"`
}

// Mail is one mail sent to one person.
type Mail struct {
	ID        uint64 `gorm:"primaryKey"`
	Recipient string `gorm:"size:320;not null;index:idx_mail_recipient_sent,priority:1"`
	// Kind is immediate or digest.
	Kind string `gorm:"size:20;not null"`
	// Type is the event type of an immediate mail (one mail bundles one type); empty for a summary.
	Type    string    `gorm:"size:100"`
	Subject string    `gorm:"size:500"`
	Count   int       `gorm:"not null"`
	SentAt  time.Time `gorm:"not null;index:idx_mail_recipient_sent,priority:2"`
}

// Preference is one person's saved settings.
type Preference struct {
	Recipient string `gorm:"primaryKey;size:320"`
	Settings  string `gorm:"type:text;not null"`
	// LastDigestAt is when the last summary went out.
	LastDigestAt *time.Time
	UpdatedAt    time.Time
}

// Store wraps the database.
type Store struct {
	db *gorm.DB
}

// New migrates the schema and returns the store.
func New(db *gorm.DB) (*Store, error) {
	if err := db.AutoMigrate(&Event{}, &Notification{}, &Mail{}, &Preference{}); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// One open event per sender and key; a resolved one no longer blocks the key. A partial index, which both PostgreSQL and SQLite know.
	if err := db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_events_open_key ON events (sender, key) WHERE resolved_at IS NULL").Error; err != nil {
		return nil, fmt.Errorf("index: %w", err)
	}
	return &Store{db: db}, nil
}

// DB is the underlying handle (tests, health check).
func (s *Store) DB() *gorm.DB { return s.db }

// Ping checks the database.
func (s *Store) Ping(ctx context.Context) error {
	sqlDB, err := s.db.DB()
	if err != nil {
		return err
	}
	return sqlDB.PingContext(ctx)
}

// ErrDuplicate is returned by InsertEvent when an open event with the same sender and key exists.
var ErrDuplicate = errors.New("duplicate event")

// InsertEvent stores e unless an open event with its sender and key exists (ErrDuplicate).
func (s *Store) InsertEvent(ctx context.Context, e *Event) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var n int64
		if err := tx.Model(&Event{}).Where("sender = ? AND key = ? AND resolved_at IS NULL", e.Sender, e.Key).Count(&n).Error; err != nil {
			return err
		}
		if n > 0 {
			return ErrDuplicate
		}
		if err := tx.Create(e).Error; err != nil {
			// Two at once: the unique index decides.
			if errors.Is(err, gorm.ErrDuplicatedKey) {
				return ErrDuplicate
			}
			return err
		}
		return nil
	})
}

// ResolveEvent closes the open event of sender and key and suppresses what of it has not gone out yet. found is false when there is none.
func (s *Store) ResolveEvent(ctx context.Context, sender, key string, now time.Time) (found bool, suppressed int64, err error) {
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ev Event
		r := tx.Where("sender = ? AND key = ? AND resolved_at IS NULL", sender, key).Limit(1).Find(&ev)
		if r.Error != nil {
			return r.Error
		}
		if r.RowsAffected == 0 {
			return nil
		}
		found = true
		if err := tx.Model(&ev).Update("resolved_at", now).Error; err != nil {
			return err
		}
		res := tx.Model(&Notification{}).Where("event_id = ? AND status IN ?", ev.ID, []string{StatusPending, StatusDigest}).
			Updates(map[string]any{"status": StatusSuppressed, "reason": "resolved"})
		suppressed = res.RowsAffected
		return res.Error
	})
	return found, suppressed, err
}

// EventsToFanOut are the events whose recipients have not been resolved yet, oldest first.
func (s *Store) EventsToFanOut(ctx context.Context, limit int) ([]Event, error) {
	var evs []Event
	err := s.db.WithContext(ctx).Where("fanned_out = ? AND resolved_at IS NULL", false).Order("id").Limit(limit).Find(&evs).Error
	return evs, err
}

// SaveFanOut stores the notifications of an event and marks it fanned out.
func (s *Store) SaveFanOut(ctx context.Context, ev *Event, ns []Notification) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(ns) > 0 {
			if err := tx.Create(&ns).Error; err != nil {
				return err
			}
		}
		return tx.Model(ev).Update("fanned_out", true).Error
	})
}

// PendingDue are the pending notifications due at now (no NotBefore or NotBefore passed), with their events.
func (s *Store) PendingDue(ctx context.Context, now time.Time) ([]Notification, error) {
	var ns []Notification
	err := s.db.WithContext(ctx).Preload("Event").
		Where("status = ? AND (not_before IS NULL OR not_before <= ?)", StatusPending, now).
		Order("recipient, type, created_at, id").Find(&ns).Error
	return ns, err
}

// DigestWaiting are the notifications waiting for a summary, with their events, ordered by recipient.
func (s *Store) DigestWaiting(ctx context.Context, recipient string) ([]Notification, error) {
	var ns []Notification
	q := s.db.WithContext(ctx).Preload("Event").Where("status = ?", StatusDigest)
	if recipient != "" {
		q = q.Where("recipient = ?", recipient)
	}
	err := q.Order("recipient, created_at, id").Find(&ns).Error
	return ns, err
}

// SetStatus sets status and reason of the notifications ids.
func (s *Store) SetStatus(ctx context.Context, ids []uint64, status, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Model(&Notification{}).Where("id IN ?", ids).
		Updates(map[string]any{"status": status, "reason": reason}).Error
}

// HoldUntil keeps the pending notifications ids back until t.
func (s *Store) HoldUntil(ctx context.Context, ids []uint64, t time.Time, reason string) error {
	if len(ids) == 0 {
		return nil
	}
	return s.db.WithContext(ctx).Model(&Notification{}).Where("id IN ?", ids).
		Updates(map[string]any{"not_before": t, "reason": reason}).Error
}

// RecordFailure counts a failed attempt on ids: held back until retryAt, or failed for good after maxAttempts.
func (s *Store) RecordFailure(ctx context.Context, ids []uint64, retryAt time.Time, maxAttempts int) (failed int64, err error) {
	if len(ids) == 0 {
		return 0, nil
	}
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&Notification{}).Where("id IN ?", ids).
			Updates(map[string]any{"attempts": gorm.Expr("attempts + 1"), "not_before": retryAt, "reason": "send_failed"}).Error; err != nil {
			return err
		}
		r := tx.Model(&Notification{}).Where("id IN ? AND attempts >= ?", ids, maxAttempts).
			Updates(map[string]any{"status": StatusFailed, "reason": "send_failed"})
		failed = r.RowsAffected
		return r.Error
	})
	return failed, err
}

// RecordMail stores m and marks the notifications ids as delivered with status (sent or digested) in one transaction.
func (s *Store) RecordMail(ctx context.Context, m *Mail, ids []uint64, status string) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(m).Error; err != nil {
			return err
		}
		return tx.Model(&Notification{}).Where("id IN ?", ids).
			Updates(map[string]any{"status": status, "sent_at": m.SentAt, "mail_id": m.ID, "not_before": nil}).Error
	})
}

// CountImmediateSince counts the immediate mails sent to recipient since t.
func (s *Store) CountImmediateSince(ctx context.Context, recipient string, t time.Time) (int, error) {
	var n int64
	err := s.db.WithContext(ctx).Model(&Mail{}).Where("recipient = ? AND kind = ? AND sent_at >= ?", recipient, "immediate", t).Count(&n).Error
	return int(n), err
}

// LastImmediateOfType is when the last immediate mail of type typ went to recipient; nil when none did.
func (s *Store) LastImmediateOfType(ctx context.Context, recipient, typ string) (*time.Time, error) {
	var m Mail
	r := s.db.WithContext(ctx).Where("recipient = ? AND kind = ? AND type = ?", recipient, "immediate", typ).Order("sent_at DESC").Limit(1).Find(&m)
	if r.Error != nil || r.RowsAffected == 0 {
		return nil, r.Error
	}
	return &m.SentAt, nil
}

// ReminderCandidates are delivered action notifications (not reminders themselves) of open events with exactly queued reminders so far, created at or before cutoff.
func (s *Store) ReminderCandidates(ctx context.Context, cutoff time.Time, queued int) ([]Notification, error) {
	var ns []Notification
	err := s.db.WithContext(ctx).
		Joins("JOIN events ON events.id = notifications.event_id").
		Where("notifications.class = ? AND notifications.reminder = 0 AND notifications.status IN ? AND notifications.reminders_queued = ? AND notifications.created_at <= ? AND events.resolved_at IS NULL",
			string(model.ClassAction), []string{StatusSent, StatusDigested}, queued, cutoff).
		Order("notifications.id").Find(&ns).Error
	return ns, err
}

// QueueReminder creates reminder r as a pending notification and counts it on the original.
func (s *Store) QueueReminder(ctx context.Context, orig *Notification, r Notification) error {
	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&r).Error; err != nil {
			return err
		}
		return tx.Model(&Notification{}).Where("id = ?", orig.ID).Update("reminders_queued", gorm.Expr("reminders_queued + 1")).Error
	})
}

// Preferences returns recipient's saved settings; ok is false when there are none.
func (s *Store) Preferences(ctx context.Context, recipient string) (p model.Preferences, lastDigest *time.Time, ok bool, err error) {
	var row Preference
	r := s.db.WithContext(ctx).Where("recipient = ?", recipient).Limit(1).Find(&row)
	if r.Error != nil {
		return p, nil, false, r.Error
	}
	if r.RowsAffected == 0 {
		return model.DefaultPreferences(), nil, false, nil
	}
	p = model.DefaultPreferences()
	// A row without settings only records the last summary.
	if row.Settings == "" {
		return p, row.LastDigestAt, false, nil
	}
	if err := json.Unmarshal([]byte(row.Settings), &p); err != nil {
		return model.DefaultPreferences(), row.LastDigestAt, false, nil
	}
	if err := p.Validate(); err != nil {
		return model.DefaultPreferences(), row.LastDigestAt, false, nil
	}
	return p, row.LastDigestAt, true, nil
}

// SavePreferences stores p for recipient, keeping when the last summary went out.
func (s *Store) SavePreferences(ctx context.Context, recipient string, p model.Preferences, now time.Time) error {
	b, err := json.Marshal(p)
	if err != nil {
		return err
	}
	row := Preference{Recipient: recipient, Settings: string(b), UpdatedAt: now}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "recipient"}},
		DoUpdates: clause.AssignmentColumns([]string{"settings", "updated_at"}),
	}).Create(&row).Error
}

// SetLastDigest records when recipient's last summary went out.
func (s *Store) SetLastDigest(ctx context.Context, recipient string, t time.Time) error {
	row := Preference{Recipient: recipient, Settings: "", LastDigestAt: &t, UpdatedAt: t}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "recipient"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_digest_at"}),
	}).Create(&row).Error
}

// Recent are the latest notifications of recipient that reached them or are about to (not suppressed), newest first, with their events.
func (s *Store) Recent(ctx context.Context, recipient string, limit int) ([]Notification, error) {
	var ns []Notification
	err := s.db.WithContext(ctx).Preload("Event").Where("recipient = ? AND status <> ?", recipient, StatusSuppressed).
		Order("created_at DESC, id DESC").Limit(limit).Find(&ns).Error
	return ns, err
}

// CountByStatus counts the notifications per status (for the metrics).
func (s *Store) CountByStatus(ctx context.Context) (map[string]int64, error) {
	type row struct {
		Status string
		N      int64
	}
	var rows []row
	if err := s.db.WithContext(ctx).Model(&Notification{}).Select("status, count(*) AS n").Where("status IN ?", []string{StatusPending, StatusDigest}).Group("status").Scan(&rows).Error; err != nil {
		return nil, err
	}
	out := map[string]int64{StatusPending: 0, StatusDigest: 0}
	for _, r := range rows {
		out[r.Status] = r.N
	}
	return out, nil
}

// DeleteBefore removes events (with their notifications) received before t and mails sent before t. An open action is kept as long as it is open.
func (s *Store) DeleteBefore(ctx context.Context, t time.Time) (int64, error) {
	var n int64
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		sub := tx.Model(&Event{}).Select("id").Where("received_at < ? AND (resolved_at IS NOT NULL OR class <> ?)", t, string(model.ClassAction))
		if err := tx.Where("event_id IN (?) AND status NOT IN ?", sub, []string{StatusPending, StatusDigest}).Delete(&Notification{}).Error; err != nil {
			return err
		}
		r := tx.Where("received_at < ? AND (resolved_at IS NOT NULL OR class <> ?) AND NOT EXISTS (SELECT 1 FROM notifications WHERE notifications.event_id = events.id)", t, string(model.ClassAction)).Delete(&Event{})
		if r.Error != nil {
			return r.Error
		}
		n = r.RowsAffected
		return tx.Where("sent_at < ?", t).Delete(&Mail{}).Error
	})
	return n, err
}
