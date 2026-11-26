package engine

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/notifications/internal/mail"
	"github.com/pfisterer/notifications/internal/model"
	"github.com/pfisterer/notifications/internal/roleprovider"
	"github.com/pfisterer/notifications/internal/store"
	"github.com/pfisterer/notifications/internal/unsub"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type harness struct {
	t   *testing.T
	e   *Engine
	rec *mail.Recorder
	now time.Time
	ctx context.Context
}

var berlin, _ = time.LoadLocation("Europe/Berlin")

// at is a local time in Berlin; 2026-11-25 is a Wednesday.
func at(day, hour, minute int) time.Time {
	return time.Date(2026, 11, day, hour, minute, 0, 0, berlin)
}

// OpenTestStore opens an in-memory SQLite database with the schema.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{TranslateError: true, Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { sqlDB.Close() })
	st, err := store.New(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func newHarness(t *testing.T, start time.Time) *harness {
	t.Helper()
	signer, _ := unsub.New(strings.Repeat("s", 32))
	h := &harness{t: t, rec: &mail.Recorder{}, now: start.UTC(), ctx: context.Background()}
	p := DefaultPolicy()
	p.Location = berlin
	h.e = &Engine{
		Store:     openTestStore(t),
		Deliverer: h.rec,
		Resolver: roleprovider.Static{
			"group:kurs#studierende": {"s1@example.edu", "s2@example.edu", "lecturer@example.edu"},
		},
		Signer: signer,
		Policy: p,
		Links:  Links{SettingsURL: "https://portal.example.edu/settings/notifications", PublicURL: "https://notify.example.edu"},
		Now:    func() time.Time { return h.now },
	}
	return h
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }
func (h *harness) setTime(t time.Time)     { h.now = t.UTC() }

func (h *harness) pass() {
	h.t.Helper()
	if err := h.e.Pass(h.ctx); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) post(sender string, r model.EventRequest) Outcome {
	h.t.Helper()
	if err := r.Validate(); err != nil {
		h.t.Fatal(err)
	}
	out, err := h.e.Ingest(h.ctx, sender, r)
	if err != nil {
		h.t.Fatal(err)
	}
	return out
}

func action(typ, key string, to ...string) model.EventRequest {
	r := model.EventRequest{Source: "openstack-management-api", Type: typ, Key: key, Class: model.ClassAction, Object: model.Object{Kind: "project", ID: key, Name: "Projekt " + key, URL: "https://portal.example.edu/cloud/" + key}}
	for _, a := range to {
		r.Recipients = append(r.Recipients, model.Recipient{User: a})
	}
	return r
}

func (h *harness) mails(to string) []mail.Message {
	var out []mail.Message
	for _, m := range h.rec.Messages() {
		if to == "" || m.To == to {
			out = append(out, m)
		}
	}
	return out
}

func (h *harness) wantMails(to string, n int) []mail.Message {
	h.t.Helper()
	ms := h.mails(to)
	if len(ms) != n {
		var subj []string
		for _, m := range ms {
			subj = append(subj, m.To+": "+m.Subject)
		}
		h.t.Fatalf("want %d mails to %q, got %d: %v", n, to, len(ms), subj)
	}
	return ms
}

const alice = "alice@example.edu"
const bob = "bob@example.edu"

func TestImmediateMailAfterCollecting(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	if out := h.post("os", action("approval.waiting", "a1", alice)); out != OutcomeAccepted {
		t.Fatalf("outcome %s", out)
	}
	h.pass()
	h.wantMails(alice, 0) // still collecting
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	m := h.wantMails(alice, 1)[0]
	if !strings.HasPrefix(m.Subject, "Antrag wartet auf Entscheidung: Projekt a1") {
		t.Errorf("subject %q", m.Subject)
	}
	if !strings.Contains(m.Headers["List-Unsubscribe"], "https://notify.example.edu/unsubscribe?token=") || m.Headers["List-Unsubscribe-Post"] != "List-Unsubscribe=One-Click" {
		t.Errorf("unsubscribe headers %v", m.Headers)
	}
	if !strings.Contains(m.Text, "https://portal.example.edu/settings/notifications") || !strings.Contains(m.HTML, "https://portal.example.edu/cloud/a1") {
		t.Error("settings link or object link missing")
	}
	if v := h.e.Metrics.Delivered.Value("test", "immediate"); v != 1 {
		t.Errorf("delivered metric %v", v)
	}
}

func TestBundlingSameTypeAndRecipient(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	for i := range 3 {
		h.post("os", action("approval.waiting", fmt.Sprintf("b%d", i), alice))
		h.advance(20 * time.Second)
	}
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	m := h.wantMails(alice, 1)[0]
	if m.Subject != "3 Anträge warten auf Entscheidung" {
		t.Errorf("subject %q", m.Subject)
	}
	if h.e.Metrics.Bundled.Value("test") != 2 {
		t.Errorf("bundled metric %v", h.e.Metrics.Bundled.Value("test"))
	}

	// Another one shortly after: waits for the 15-minute window, then goes out on its own; one of another type is not held back.
	h.advance(time.Minute)
	h.post("os", action("approval.waiting", "b3", alice))
	h.post("os", action("project.ending", "p1", alice))
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	ms := h.wantMails(alice, 2)
	if !strings.HasPrefix(ms[1].Subject, "Projekt endet bald") {
		t.Errorf("second mail %q", ms[1].Subject)
	}
	h.advance(5 * time.Minute)
	h.pass()
	h.wantMails(alice, 2)
	h.advance(10 * time.Minute)
	h.pass()
	ms = h.wantMails(alice, 3)
	if !strings.Contains(ms[2].Subject, "Projekt b3") {
		t.Errorf("third mail %q", ms[2].Subject)
	}
}

func TestDailyLimitMovesTheRestToTheSummary(t *testing.T) {
	h := newHarness(t, at(25, 9, 0))
	for i := range 7 {
		// Different types, so bundling does not merge them.
		h.post("os", action(fmt.Sprintf("custom.t%d", i), fmt.Sprintf("d%d", i), alice))
	}
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 5)
	waiting, _ := h.e.Store.DigestWaiting(h.ctx, alice)
	if len(waiting) != 2 || waiting[0].Reason != "daily_limit" {
		t.Fatalf("want 2 notifications deferred for the daily limit, got %+v", waiting)
	}
	if h.e.Metrics.Deferred.Value("daily_limit") != 2 {
		t.Errorf("deferred metric %v", h.e.Metrics.Deferred.Value("daily_limit"))
	}
	// The summary with the actions goes out at the next daily slot, although the default cadence is weekly.
	h.setTime(at(26, 7, 59))
	h.pass()
	h.wantMails(alice, 5)
	h.setTime(at(26, 8, 0))
	h.pass()
	ms := h.wantMails(alice, 6)
	if !strings.HasPrefix(ms[5].Subject, "Zusammenfassung: 2 Benachrichtigungen") {
		t.Errorf("summary subject %q", ms[5].Subject)
	}
	// A new day: immediate mails again.
	h.post("os", action("custom.t0", "d-next", alice))
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 7)
}

// There are no quiet hours: a mail at night or on the weekend goes out as soon as it is due.
func TestNoQuietHours(t *testing.T) {
	for name, start := range map[string]time.Time{"night": at(25, 23, 0), "saturday": at(28, 11, 0)} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, start)
			h.post("os", action("approval.waiting", "n1", alice))
			h.advance(h.e.Policy.BundleCollect)
			h.pass()
			h.wantMails(alice, 1)
		})
	}
}

func TestRemindersForOpenActions(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	h.post("os", action("approval.waiting", "r1", alice))
	h.post("os", action("approval.waiting", "r2", bob))
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 1)
	h.wantMails(bob, 1)
	// Bob's request is decided: no reminders for him.
	if out := h.post("os", model.EventRequest{Type: "approval.waiting", Key: "r2", Resolve: true}); out != OutcomeResolved {
		t.Fatalf("resolve: %s", out)
	}

	h.setTime(at(27, 10, 5)) // two days later
	h.pass()
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	ms := h.wantMails(alice, 2)
	if !strings.HasPrefix(ms[1].Subject, "Erinnerung: Antrag wartet") {
		t.Errorf("reminder subject %q", ms[1].Subject)
	}
	h.wantMails(bob, 1)

	h.setTime(at(30, 10, 5)) // Monday, five days after: nothing new
	h.pass()
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 2)

	h.setTime(time.Date(2026, 12, 2, 10, 5, 0, 0, berlin)) // seven days after
	h.pass()
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 3)

	// Never more than two.
	h.setTime(time.Date(2026, 12, 21, 10, 5, 0, 0, berlin))
	h.pass()
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 3)
}

func TestResolveDropsWhatHasNotGoneOut(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	h.post("os", action("approval.waiting", "x1", alice))
	h.post("os", model.EventRequest{Type: "approval.waiting", Key: "x1", Resolve: true})
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 0)
	// A resolve without an open event is answered, not refused.
	if out := h.post("os", model.EventRequest{Type: "approval.waiting", Key: "never", Resolve: true}); out != OutcomeNothingToResolve {
		t.Errorf("outcome %s", out)
	}
}

func TestNeverAboutOwnActions(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	r := action("approval.decided", "o1", alice, bob)
	r.Class = model.ClassAction
	r.Actor = alice
	h.post("os", r)
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 0)
	h.wantMails(bob, 1)
	if h.e.Metrics.Suppressed.Value("own_action") != 1 {
		t.Errorf("suppressed metric %v", h.e.Metrics.Suppressed.Value("own_action"))
	}
}

func TestDuplicatesAreSuppressed(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	if out := h.post("os", action("approval.waiting", "dup", alice)); out != OutcomeAccepted {
		t.Fatal(out)
	}
	if out := h.post("os", action("approval.waiting", "dup", alice)); out != OutcomeDuplicate {
		t.Fatalf("second: %s", out)
	}
	// The same key from another service is that service's own event.
	if out := h.post("other", action("approval.waiting", "dup", bob)); out != OutcomeAccepted {
		t.Fatalf("other sender: %s", out)
	}
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails(alice, 1)
	h.wantMails(bob, 1)
	if h.e.Metrics.Suppressed.Value("duplicate") != 1 {
		t.Errorf("suppressed metric %v", h.e.Metrics.Suppressed.Value("duplicate"))
	}
	// Once resolved, the key may come again.
	h.post("os", model.EventRequest{Type: "approval.waiting", Key: "dup", Resolve: true})
	if out := h.post("os", action("approval.waiting", "dup", alice)); out != OutcomeAccepted {
		t.Fatalf("after resolve: %s", out)
	}
}

// collectDecisions saves preferences for who that put approval.decided into the summary, as a person may choose.
func (h *harness) collectDecisions(who string) {
	h.t.Helper()
	p := model.DefaultPreferences()
	p.Types["approval.decided"] = model.ModeDigest
	if err := h.e.Store.SavePreferences(h.ctx, who, p, h.now); err != nil {
		h.t.Fatal(err)
	}
}

func TestInfoOnlyInTheSummary(t *testing.T) {
	h := newHarness(t, at(25, 10, 0)) // Wednesday
	h.collectDecisions(alice)
	r := action("approval.decided", "i1", alice)
	r.Class = model.ClassInfo
	r.Data = map[string]any{"decision": "approved", "decided_by": "admin@example.edu"}
	h.post("os", r)
	h.advance(time.Hour)
	h.pass()
	h.wantMails(alice, 0)
	// Weekly by default: Monday at eight.
	h.setTime(at(30, 7, 59))
	h.pass()
	h.wantMails(alice, 0)
	h.setTime(at(30, 8, 0))
	h.pass()
	m := h.wantMails(alice, 1)[0]
	if !strings.HasPrefix(m.Subject, "Zusammenfassung: 1 Benachrichtigung") || !strings.Contains(m.Text, "Antrag genehmigt: Projekt i1") {
		t.Errorf("summary %q\n%s", m.Subject, m.Text)
	}
	if h.e.Metrics.Delivered.Value("test", "digest") != 1 {
		t.Errorf("digest metric %v", h.e.Metrics.Delivered.Value("test", "digest"))
	}
	// Nothing new: no empty summary next week.
	h.setTime(time.Date(2026, 12, 7, 8, 0, 0, 0, berlin))
	h.pass()
	h.wantMails(alice, 1)
}

// An info without a mode of its own stays in the summary by default; approval.decided comes at once unless the person collects it or has switched info off.
func TestDecisionComesAtOnceOtherInfoWaits(t *testing.T) {
	h := newHarness(t, at(25, 10, 0)) // Wednesday
	d := action("approval.decided", "d1", alice)
	d.Class = model.ClassInfo
	d.Data = map[string]any{"decision": "approved", "decided_by": "admin@example.edu"}
	h.post("os", d)
	o := action("budget.created", "o1", alice)
	o.Class = model.ClassInfo
	h.post("os", o)
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	ms := h.wantMails(alice, 1)
	if !strings.Contains(ms[0].Subject, "Antrag genehmigt: Projekt d1") {
		t.Errorf("subject %q", ms[0].Subject)
	}
	// The other info waits for Monday's summary.
	h.setTime(at(30, 8, 0))
	h.pass()
	ms = h.wantMails(alice, 2)
	if !strings.HasPrefix(ms[1].Subject, "Zusammenfassung: 1 Benachrichtigung") {
		t.Errorf("summary %q", ms[1].Subject)
	}

	// Info switched off: no decision either, unless chosen for the type itself.
	h2 := newHarness(t, at(25, 10, 0))
	p := model.DefaultPreferences()
	p.Classes[model.ClassInfo] = model.ModeOff
	if err := h2.e.Store.SavePreferences(h2.ctx, bob, p, h2.now); err != nil {
		t.Fatal(err)
	}
	d2 := action("approval.decided", "d2", bob)
	d2.Class = model.ClassInfo
	h2.post("os", d2)
	h2.advance(h2.e.Policy.BundleCollect)
	h2.pass()
	h2.wantMails(bob, 0)
	if h2.e.Metrics.Suppressed.Value("off") != 1 {
		t.Errorf("off metric %v", h2.e.Metrics.Suppressed.Value("off"))
	}
}

func TestSendDigestsNow(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	h.collectDecisions(alice)
	for i := range 2 {
		r := action("approval.decided", fmt.Sprintf("s%d", i), alice)
		r.Class = model.ClassInfo
		h.post("os", r)
	}
	h.pass()
	n, err := h.e.SendDigests(h.ctx, alice)
	if err != nil || n != 1 {
		t.Fatalf("sent %d, %v", n, err)
	}
	h.wantMails(alice, 1)
}

func TestGroupsAreResolvedOnce(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	r := action("allocation.expired", "g1")
	r.Recipients = []model.Recipient{{Group: "group:kurs#studierende"}, {User: "s1@example.edu"}}
	r.Actor = "lecturer@example.edu"
	h.post("os", r)
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	h.wantMails("s1@example.edu", 1)
	h.wantMails("s2@example.edu", 1)
	h.wantMails("lecturer@example.edu", 0)
}

func TestPreferencesUnsubscribedAndOff(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	p := model.DefaultPreferences()
	p.Unsubscribed = []string{"project.ending"}
	p.Classes[model.ClassDisruption] = model.ModeOff
	if err := h.e.Store.SavePreferences(h.ctx, alice, p, h.now); err != nil {
		t.Fatal(err)
	}
	h.post("os", action("project.ending", "u1", alice))
	d := action("allocation.shortened", "u2", alice)
	d.Class = model.ClassDisruption
	h.post("os", d)
	h.post("os", action("approval.waiting", "u3", alice))
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	ms := h.wantMails(alice, 1)
	if !strings.Contains(ms[0].Subject, "Antrag wartet") {
		t.Errorf("subject %q", ms[0].Subject)
	}
	if h.e.Metrics.Suppressed.Value("unsubscribed") != 1 || h.e.Metrics.Suppressed.Value("off") != 1 {
		t.Errorf("suppressed metrics unsubscribed=%v off=%v", h.e.Metrics.Suppressed.Value("unsubscribed"), h.e.Metrics.Suppressed.Value("off"))
	}
}

func TestEnglishMails(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	p := model.DefaultPreferences()
	p.Language = "en"
	_ = h.e.Store.SavePreferences(h.ctx, alice, p, h.now)
	r := action("allocation.expired", "e1", alice)
	r.Data = map[string]any{"budget": "DSKI", "shrink_at": "2026-12-09"}
	h.post("os", r)
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	m := h.wantMails(alice, 1)[0]
	if m.Subject != "Allocation expired: Projekt e1" || !strings.Contains(m.Text, "On 9 December 2026") {
		t.Errorf("%q\n%s", m.Subject, m.Text)
	}
}

func TestFailedSendIsRetried(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	h.rec.Fail = fmt.Errorf("connection refused")
	h.post("os", action("approval.waiting", "f1", alice))
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	if h.e.Metrics.Failed.Value("send") != 1 {
		t.Errorf("failed metric %v", h.e.Metrics.Failed.Value("send"))
	}
	h.rec.Fail = nil
	h.pass()
	h.wantMails(alice, 0) // backing off
	h.advance(time.Minute)
	h.pass()
	h.wantMails(alice, 1)
}

func TestGroupResolutionFailureWaits(t *testing.T) {
	h := newHarness(t, at(25, 10, 0))
	h.e.Resolver = failing{}
	r := action("allocation.expired", "w1")
	r.Recipients = []model.Recipient{{Group: "group:kurs"}}
	h.post("os", r)
	h.advance(h.e.Policy.BundleCollect)
	h.pass()
	if h.e.Metrics.Failed.Value("resolve") != 1 {
		t.Errorf("resolve failures %v", h.e.Metrics.Failed.Value("resolve"))
	}
	h.e.Resolver = roleprovider.Static{"group:kurs": {alice}}
	h.pass()
	h.advance(time.Second)
	h.pass()
	h.wantMails(alice, 1)
}

type failing struct{}

func (failing) Members(context.Context, string) ([]string, error) { return nil, fmt.Errorf("down") }

func TestObjectsWithoutURLGetThePortalLink(t *testing.T) {
	l := Links{PortalURL: "https://portal.example.edu/"}
	for kind, want := range map[string]string{"project": "https://portal.example.edu/projects?project=p_1", "budget": "https://portal.example.edu/projects?budget=p_1", "allocation": "https://portal.example.edu/projects?node=p_1"} {
		if got := l.Complete(model.Object{Kind: kind, ID: "p_1"}).URL; got != want {
			t.Errorf("%s: %s", kind, got)
		}
	}
	if got := l.Complete(model.Object{Kind: "project", ID: "p_1", URL: "https://elsewhere.example.edu/x"}).URL; got != "https://elsewhere.example.edu/x" {
		t.Errorf("a given url is kept: %s", got)
	}
	if got := (Links{}).Complete(model.Object{Kind: "project", ID: "p_1"}).URL; got != "" {
		t.Errorf("without a portal no link: %s", got)
	}
}
