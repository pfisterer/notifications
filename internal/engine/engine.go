// Package engine decides whether, when and how an event reaches people: it takes events in, resolves their recipients, and delivers the notifications under the rules of Policy — at once (bundled per type, within a daily limit), in the summary, or not at all — and reminds of open actions.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/pfisterer/cloud-self-service-golib/loop"
	"github.com/pfisterer/notifications/internal/mail"
	"github.com/pfisterer/notifications/internal/model"
	"github.com/pfisterer/notifications/internal/roleprovider"
	"github.com/pfisterer/notifications/internal/store"
	"github.com/pfisterer/notifications/internal/texts"
	"github.com/pfisterer/notifications/internal/unsub"
	"go.uber.org/zap"
)

// Links are the addresses the mails point to.
type Links struct {
	// SettingsURL is the portal page with the notification settings.
	SettingsURL string
	// PublicURL is where this service is reached from outside, for the unsubscribe links (…/unsubscribe?token=…).
	PublicURL string
	// PortalURL is the self-service portal: an object sent without a url gets the portal's link to it (Complete).
	PortalURL string
}

// Complete gives an object without a url the portal's address of it, which the portal sends on to where the viewer sees it: /projects?project=<id> for a project, /projects?budget=<id> for a budget, /projects?node=<id> for anything else of the budget tree (an allocation, a request). An object with a url, or without an id, stays as it is.
func (l Links) Complete(o model.Object) model.Object {
	if o.URL != "" || o.ID == "" || l.PortalURL == "" {
		return o
	}
	param := "node"
	switch o.Kind {
	case "project":
		param = "project"
	case "budget":
		param = "budget"
	}
	o.URL = strings.TrimRight(l.PortalURL, "/") + "/projects?" + param + "=" + url.QueryEscape(o.ID)
	return o
}

// Engine is the service's core.
type Engine struct {
	Store     *store.Store
	Deliverer mail.Deliverer
	Resolver  roleprovider.Resolver
	Signer    *unsub.Signer
	Policy    Policy
	Links     Links
	Metrics   *Metrics
	Log       *zap.SugaredLogger
	// Now is the clock; tests set it.
	Now func() time.Time

	mu          sync.Mutex
	nudge       chan struct{}
	lastCleanup time.Time
	digestRetry map[string]time.Time
	initOnce    sync.Once
}

func (e *Engine) init() {
	e.initOnce.Do(func() {
		e.nudge = make(chan struct{}, 1)
		e.digestRetry = map[string]time.Time{}
		if e.Now == nil {
			e.Now = time.Now
		}
		if e.Metrics == nil {
			e.Metrics = NewMetrics()
		}
		if e.Log == nil {
			e.Log = zap.NewNop().Sugar()
		}
		if e.Policy.Location == nil {
			e.Policy.Location = time.UTC
		}
	})
}

// Outcome is what became of a posted event.
type Outcome string

const (
	OutcomeAccepted  Outcome = "accepted"
	OutcomeDuplicate Outcome = "duplicate"
	OutcomeResolved  Outcome = "resolved"
	// OutcomeNothingToResolve: a resolve for a key with no open event.
	OutcomeNothingToResolve Outcome = "nothing_to_resolve"
)

// Ingest takes a validated event from the service token named sender.
func (e *Engine) Ingest(ctx context.Context, sender string, r model.EventRequest) (Outcome, error) {
	e.init()
	now := e.Now().UTC()
	if r.Resolve {
		found, n, err := e.Store.ResolveEvent(ctx, sender, r.Key, now)
		if err != nil {
			return "", err
		}
		e.Metrics.add(e.Metrics.Suppressed, int(n), "resolved")
		if !found {
			return OutcomeNothingToResolve, nil
		}
		return OutcomeResolved, nil
	}
	source := r.Source
	if source == "" {
		source = sender
	}
	obj, _ := json.Marshal(r.Object)
	data, _ := json.Marshal(r.Data)
	if r.Data == nil {
		data = []byte("{}")
	}
	rcpt, _ := json.Marshal(r.Recipients)
	ev := &store.Event{
		Sender: sender, Source: source, Type: r.Type, Key: r.Key, Class: string(r.Class), Actor: r.Actor,
		Object: string(obj), Data: string(data), Recipients: string(rcpt), ReceivedAt: now,
	}
	if r.Due != nil {
		d := r.Due.UTC()
		ev.Due = &d
	}
	if err := e.Store.InsertEvent(ctx, ev); err != nil {
		if errors.Is(err, store.ErrDuplicate) {
			e.Metrics.Suppressed.Inc("duplicate")
			return OutcomeDuplicate, nil
		}
		return "", err
	}
	e.Metrics.Received.Inc(sender, string(r.Class))
	e.Nudge()
	return OutcomeAccepted, nil
}

// Nudge makes the loop run a pass soon.
func (e *Engine) Nudge() {
	e.init()
	select {
	case e.nudge <- struct{}{}:
	default:
	}
}

// Run runs the delivery loop until ctx ends: every interval and after every accepted event.
func (e *Engine) Run(ctx context.Context, interval time.Duration) {
	e.init()
	loop.Run(ctx, loop.Schedule{Interval: interval, Nudges: []<-chan struct{}{e.nudge}}, e.Pass, loop.Options{
		Name: "dispatch", Log: e.Log,
		OnPanic: func(any, []byte) { e.Metrics.Panics.Inc("dispatch") },
	})
}

// Pass does one round: fan out new events, queue reminders, deliver what is due at once, send the summaries that are due, and now and then delete what is old.
func (e *Engine) Pass(ctx context.Context) error {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	var errs []error
	for _, step := range []struct {
		name string
		fn   func(context.Context) error
	}{{"fan-out", e.fanOut}, {"reminders", e.reminders}, {"dispatch", e.dispatch}, {"digests", func(ctx context.Context) error { _, err := e.digests(ctx, false, ""); return err }}} {
		if err := step.fn(ctx); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", step.name, err))
		}
	}
	now := e.Now()
	if e.Policy.Retention > 0 && now.Sub(e.lastCleanup) > time.Hour {
		e.lastCleanup = now
		if n, err := e.Store.DeleteBefore(ctx, now.Add(-e.Policy.Retention)); err != nil {
			errs = append(errs, fmt.Errorf("cleanup: %w", err))
		} else if n > 0 {
			e.Log.Infow("old events deleted", "events", n)
		}
	}
	e.Metrics.passDone(now)
	return errors.Join(errs...)
}

// fanOut turns the recipients of new events into notifications, one per person, never for the actor.
func (e *Engine) fanOut(ctx context.Context) error {
	evs, err := e.Store.EventsToFanOut(ctx, 200)
	if err != nil {
		return err
	}
	for i := range evs {
		ev := &evs[i]
		seen := map[string]bool{}
		var people []string
		failed := false
		for _, r := range ev.RecipientsValue() {
			var addrs []string
			if r.User != "" {
				addrs = []string{r.User}
			} else {
				m, err := e.Resolver.Members(ctx, r.Group)
				if err != nil {
					// Tried again in the next pass; the event waits for it as a whole, so nobody gets it twice.
					e.Metrics.Failed.Inc("resolve")
					e.Log.Warnw("resolving a group failed, retrying later", "event", ev.ID, "group", r.Group, "error", err)
					failed = true
					break
				}
				addrs = m
			}
			for _, a := range addrs {
				a = strings.ToLower(strings.TrimSpace(a))
				if a == "" || seen[a] {
					continue
				}
				seen[a] = true
				people = append(people, a)
			}
		}
		if failed {
			continue
		}
		var ns []store.Notification
		for _, p := range people {
			if ev.Actor != "" && p == ev.Actor {
				e.Metrics.Suppressed.Inc("own_action")
				continue
			}
			ns = append(ns, store.Notification{EventID: ev.ID, Recipient: p, Type: ev.Type, Class: ev.Class, Status: store.StatusPending, CreatedAt: ev.ReceivedAt})
		}
		if len(ns) == 0 {
			e.Metrics.Suppressed.Inc("no_recipient")
		}
		if err := e.Store.SaveFanOut(ctx, ev, ns); err != nil {
			return err
		}
	}
	return nil
}

// reminders queues the reminders of open actions that are due.
func (e *Engine) reminders(ctx context.Context) error {
	now := e.Now().UTC()
	for i, after := range e.Policy.ReminderAfter {
		cands, err := e.Store.ReminderCandidates(ctx, now.Add(-after), i)
		if err != nil {
			return err
		}
		for j := range cands {
			c := &cands[j]
			r := store.Notification{EventID: c.EventID, Recipient: c.Recipient, Type: c.Type, Class: c.Class, Reminder: i + 1, Status: store.StatusPending, CreatedAt: now}
			if err := e.Store.QueueReminder(ctx, c, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// group is one person's pending notifications of one type.
type group struct {
	recipient string
	typ       string
	items     []store.Notification
}

func groupPending(ns []store.Notification) [][]group {
	var out [][]group
	byRecipient := map[string]int{}
	byType := map[string]int{}
	for _, n := range ns {
		ri, ok := byRecipient[n.Recipient]
		if !ok {
			ri = len(out)
			byRecipient[n.Recipient] = ri
			out = append(out, nil)
		}
		k := n.Recipient + "\x00" + n.Type
		gi, ok := byType[k]
		if !ok {
			gi = len(out[ri])
			byType[k] = gi
			out[ri] = append(out[ri], group{recipient: n.Recipient, typ: n.Type})
		}
		out[ri][gi].items = append(out[ri][gi].items, n)
	}
	return out
}

func ids(ns []store.Notification) []uint64 {
	out := make([]uint64, len(ns))
	for i, n := range ns {
		out[i] = n.ID
	}
	return out
}

// dispatch delivers what is due at once, person by person and type by type.
func (e *Engine) dispatch(ctx context.Context) error {
	now := e.Now().UTC()
	pending, err := e.Store.PendingDue(ctx, now)
	if err != nil {
		return err
	}
	for _, perRecipient := range groupPending(pending) {
		recipient := perRecipient[0].recipient
		prefs, _, _, err := e.Store.Preferences(ctx, recipient)
		if err != nil {
			return err
		}
		sentToday, err := e.Store.CountImmediateSince(ctx, recipient, DayStart(now, e.Policy.Location).UTC())
		if err != nil {
			return err
		}
		for _, g := range perRecipient {
			sent, err := e.dispatchGroup(ctx, now, prefs, sentToday, g)
			if err != nil {
				return err
			}
			if sent {
				sentToday++
			}
		}
	}
	return nil
}

// dispatchGroup applies the rules to one person's pending notifications of one type and sends them as one message when they are due; sent says whether it did.
func (e *Engine) dispatchGroup(ctx context.Context, now time.Time, prefs model.Preferences, sentToday int, g group) (bool, error) {
	var immediate, toDigest, resolved, unsubscribed, off []store.Notification
	for _, n := range g.items {
		switch {
		case n.Event != nil && n.Event.ResolvedAt != nil:
			resolved = append(resolved, n)
		case prefs.IsUnsubscribed(n.Type):
			unsubscribed = append(unsubscribed, n)
		default:
			switch prefs.ModeOf(model.Class(n.Class)) {
			case model.ModeOff:
				off = append(off, n)
			case model.ModeDigest:
				toDigest = append(toDigest, n)
			default:
				immediate = append(immediate, n)
			}
		}
	}
	for _, s := range []struct {
		ns     []store.Notification
		reason string
	}{{resolved, "resolved"}, {unsubscribed, "unsubscribed"}, {off, "off"}} {
		if len(s.ns) == 0 {
			continue
		}
		if err := e.Store.SetStatus(ctx, ids(s.ns), store.StatusSuppressed, s.reason); err != nil {
			return false, err
		}
		e.Metrics.add(e.Metrics.Suppressed, len(s.ns), s.reason)
	}
	if err := e.Store.SetStatus(ctx, ids(toDigest), store.StatusDigest, "preference"); err != nil {
		return false, err
	}
	if len(immediate) == 0 {
		return false, nil
	}

	oldest := immediate[0].CreatedAt
	for _, n := range immediate {
		if n.CreatedAt.Before(oldest) {
			oldest = n.CreatedAt
		}
	}
	// Bundling: wait a moment for more of the same, and after a mail of this type wait for the window to pass.
	if now.Before(oldest.Add(e.Policy.BundleCollect)) {
		return false, nil
	}
	last, err := e.Store.LastImmediateOfType(ctx, g.recipient, g.typ)
	if err != nil {
		return false, err
	}
	if last != nil && now.Before(last.Add(e.Policy.BundleWindow)) {
		return false, nil
	}
	if e.Policy.DailyLimit > 0 && sentToday >= e.Policy.DailyLimit {
		e.Metrics.add(e.Metrics.Deferred, len(immediate), "daily_limit")
		return false, e.Store.SetStatus(ctx, ids(immediate), store.StatusDigest, "daily_limit")
	}
	return e.sendImmediate(ctx, now, prefs, g.recipient, g.typ, immediate)
}

func (e *Engine) view(n store.Notification) texts.View {
	v := texts.View{Type: n.Type, Reminder: n.Reminder, Loc: e.Policy.Location}
	if n.Event != nil {
		v.Object = e.Links.Complete(n.Event.ObjectValue())
		v.Data = n.Event.DataValue()
		v.Due = n.Event.Due
	}
	return v
}

func (e *Engine) unsubscribeURL(recipient, what string) string {
	if e.Signer == nil || e.Links.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(e.Links.PublicURL, "/") + "/unsubscribe?token=" + url.QueryEscape(e.Signer.Sign(unsub.Claim{Recipient: recipient, Type: what}))
}

func (e *Engine) message(recipient string, c texts.Content, unsubURL string) mail.Message {
	m := mail.Message{To: recipient, Subject: c.Subject, Text: c.Text, HTML: c.HTML, Headers: map[string]string{}}
	if unsubURL != "" {
		// RFC 2369 and RFC 8058: mail programs offer an unsubscribe button, and a POST to the link ends the mails without a click on a page.
		m.Headers["List-Unsubscribe"] = "<" + unsubURL + ">"
		m.Headers["List-Unsubscribe-Post"] = "List-Unsubscribe=One-Click"
	}
	return m
}

func (e *Engine) sendImmediate(ctx context.Context, now time.Time, prefs model.Preferences, recipient, typ string, ns []store.Notification) (bool, error) {
	l := texts.ParseLang(prefs.Language)
	items := make([]texts.Item, len(ns))
	reminders := true
	maxAttempts := 0
	for i, n := range ns {
		items[i] = texts.Render(e.view(n), l)
		if n.Reminder == 0 {
			reminders = false
		}
		if n.Attempts > maxAttempts {
			maxAttempts = n.Attempts
		}
	}
	unsubURL := e.unsubscribeURL(recipient, typ)
	k := texts.KindOf(typ)
	what := map[texts.Lang]string{texts.DE: "Benachrichtigungen dieser Art (" + k.Name(texts.DE) + ")", texts.EN: "notifications of this kind (" + k.Name(texts.EN) + ")"}[l]
	content := texts.Immediate(l, typ, items, reminders, texts.Footer{SettingsURL: e.Links.SettingsURL, UnsubscribeURL: unsubURL, UnsubscribeWhat: what})
	msg := e.message(recipient, content, unsubURL)
	channel := e.Deliverer.Channel()
	if err := e.Deliverer.Deliver(ctx, msg); err != nil {
		e.Metrics.Failed.Inc("send")
		e.Log.Warnw("sending failed, retrying later", "type", typ, "notifications", len(ns), "error", err)
		gaveUp, ferr := e.Store.RecordFailure(ctx, ids(ns), now.Add(e.Policy.backoff(maxAttempts)), e.Policy.MaxAttempts)
		e.Metrics.add(e.Metrics.Failed, int(gaveUp), "gave_up")
		return false, ferr
	}
	if err := e.Store.RecordMail(ctx, &store.Mail{Recipient: recipient, Kind: "immediate", Type: typ, Subject: content.Subject, Count: len(ns), SentAt: now}, ids(ns), store.StatusSent); err != nil {
		return true, err
	}
	e.Metrics.Messages.Inc(channel, "immediate")
	e.Metrics.add(e.Metrics.Delivered, len(ns), channel, "immediate")
	e.Metrics.add(e.Metrics.Bundled, len(ns)-1, channel)
	return true, nil
}

// SendDigests sends the summaries now, of one person or (recipient "") of everybody with something waiting, regardless of the schedule; for the admin endpoint and tests. It returns how many went out.
func (e *Engine) SendDigests(ctx context.Context, recipient string) (int, error) {
	e.init()
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.digests(ctx, true, recipient)
}

// digests sends each person's summary when its time has come (force: now).
func (e *Engine) digests(ctx context.Context, force bool, only string) (int, error) {
	now := e.Now().UTC()
	waiting, err := e.Store.DigestWaiting(ctx, only)
	if err != nil {
		return 0, err
	}
	byRecipient := map[string][]store.Notification{}
	var order []string
	for _, n := range waiting {
		if _, ok := byRecipient[n.Recipient]; !ok {
			order = append(order, n.Recipient)
		}
		byRecipient[n.Recipient] = append(byRecipient[n.Recipient], n)
	}
	sent := 0
	for _, r := range order {
		ns := byRecipient[r]
		prefs, last, _, err := e.Store.Preferences(ctx, r)
		if err != nil {
			return sent, err
		}
		if !force {
			if t, ok := e.digestRetry[r]; ok && now.Before(t) {
				continue
			}
			cadence := prefs.Digest
			ref := ns[0].CreatedAt
			for _, n := range ns {
				// An action or disruption that landed here (daily limit, or the person collects them) does not wait a week.
				if n.Class != string(model.ClassInfo) {
					cadence = model.DigestDaily
				}
				if n.CreatedAt.Before(ref) {
					ref = n.CreatedAt
				}
			}
			if last != nil && last.After(ref) {
				ref = *last
			}
			if now.Before(NextDigestSlot(ref, cadence, e.Policy.DigestHour, e.Policy.Location)) {
				continue
			}
		}
		ok, err := e.sendDigest(ctx, now, prefs, r, ns)
		if err != nil {
			return sent, err
		}
		if ok {
			sent++
		}
	}
	return sent, nil
}

func (e *Engine) sendDigest(ctx context.Context, now time.Time, prefs model.Preferences, recipient string, ns []store.Notification) (bool, error) {
	var keep, resolved, unsubscribed, off []store.Notification
	for _, n := range ns {
		switch {
		case n.Event != nil && n.Event.ResolvedAt != nil:
			resolved = append(resolved, n)
		case prefs.IsUnsubscribed(n.Type):
			unsubscribed = append(unsubscribed, n)
		case prefs.ModeOf(model.Class(n.Class)) == model.ModeOff:
			off = append(off, n)
		default:
			keep = append(keep, n)
		}
	}
	for _, s := range []struct {
		ns     []store.Notification
		reason string
	}{{resolved, "resolved"}, {unsubscribed, "unsubscribed"}, {off, "off"}} {
		if len(s.ns) == 0 {
			continue
		}
		if err := e.Store.SetStatus(ctx, ids(s.ns), store.StatusSuppressed, s.reason); err != nil {
			return false, err
		}
		e.Metrics.add(e.Metrics.Suppressed, len(s.ns), s.reason)
	}
	if len(keep) == 0 {
		return false, nil
	}
	l := texts.ParseLang(prefs.Language)
	// One section per type, actions first, then disruptions, then infos; within a class by type.
	classRank := map[string]int{string(model.ClassAction): 0, string(model.ClassDisruption): 1, string(model.ClassInfo): 2}
	byType := map[string][]store.Notification{}
	for _, n := range keep {
		byType[n.Type] = append(byType[n.Type], n)
	}
	types := make([]string, 0, len(byType))
	for t := range byType {
		types = append(types, t)
	}
	sort.Slice(types, func(i, j int) bool {
		ci, cj := classRank[byType[types[i]][0].Class], classRank[byType[types[j]][0].Class]
		if ci != cj {
			return ci < cj
		}
		return types[i] < types[j]
	})
	var sections []texts.Section
	for _, t := range types {
		var items []texts.Item
		for _, n := range byType[t] {
			items = append(items, texts.Render(e.view(n), l))
		}
		sections = append(sections, texts.Section{Heading: fmt.Sprintf("%s (%d)", texts.KindOf(t).Name(l), len(items)), Items: items})
	}
	unsubURL := e.unsubscribeURL(recipient, unsub.InfoClass)
	what := map[texts.Lang]string{texts.DE: "Zusammenfassungen mit Infos", texts.EN: "summaries of information"}[l]
	content := texts.Digest(l, sections, texts.Footer{SettingsURL: e.Links.SettingsURL, UnsubscribeURL: unsubURL, UnsubscribeWhat: what})
	channel := e.Deliverer.Channel()
	if err := e.Deliverer.Deliver(ctx, e.message(recipient, content, unsubURL)); err != nil {
		e.Metrics.Failed.Inc("send")
		e.Log.Warnw("sending a summary failed, retrying later", "notifications", len(keep), "error", err)
		e.digestRetry[recipient] = now.Add(5 * time.Minute)
		return false, nil
	}
	delete(e.digestRetry, recipient)
	if err := e.Store.RecordMail(ctx, &store.Mail{Recipient: recipient, Kind: "digest", Subject: content.Subject, Count: len(keep), SentAt: now}, ids(keep), store.StatusDigested); err != nil {
		return true, err
	}
	if err := e.Store.SetLastDigest(ctx, recipient, now); err != nil {
		return true, err
	}
	e.Metrics.Messages.Inc(channel, "digest")
	e.Metrics.add(e.Metrics.Delivered, len(keep), channel, "digest")
	return true, nil
}
