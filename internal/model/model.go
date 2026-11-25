// Package model holds the types of the notification service's contract: the event a service posts, the classes that decide how urgently it reaches people, and a person's preferences.
package model

import (
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Class says how urgently an event concerns its recipients.
type Class string

const (
	// ClassAction: somebody has to act (a request waits, a deletion day approaches). Delivered at once, reminded of while open.
	ClassAction Class = "action"
	// ClassDisruption: something that affects the recipient went wrong (a project could not be created, a workspace was evicted). Delivered at once.
	ClassDisruption Class = "disruption"
	// ClassInfo: good to know (approved, a new budget). Only in the summary.
	ClassInfo Class = "info"
)

// Classes are all classes, in the order the settings show them.
var Classes = []Class{ClassAction, ClassDisruption, ClassInfo}

// Valid says whether c is one of the three classes.
func (c Class) Valid() bool { return slices.Contains(Classes, c) }

// Recipient is one entry of an event's recipients: exactly one of User (a mail address) and Group (a role-provider userset, "group:<id>" or "group:<id>#<relation>").
type Recipient struct {
	User  string `json:"user,omitempty" example:"someone@example.edu"`
	Group string `json:"group,omitempty" example:"group:dualis-ma-wdski25a#studierende"`
}

// Object is what an event is about; its URL is where the mail sends people.
type Object struct {
	Kind string `json:"kind,omitempty" example:"project"`
	ID   string `json:"id,omitempty" example:"p_123"`
	Name string `json:"name,omitempty" example:"Data Science WS 25"`
	URL  string `json:"url,omitempty" example:"https://self-service.staging.dhbw.cloud/cloud/projects/p_123"`
}

// EventRequest is the body of POST /v1/events.
type EventRequest struct {
	// Source names the sending service. Duplicates are recognised per sending token, so one service cannot resolve another's events.
	Source string `json:"source" example:"openstack-management-api"`
	// Type is the kind of event; it selects the text. Unknown types are delivered with a generic text (data.title, data.text).
	Type string `json:"type" example:"allocation.expired"`
	// Key identifies the event against duplicates: while an event with this key is open, another one with the same key is a duplicate. resolve=true with the same key closes it.
	Key string `json:"key" example:"allocation-expired:a_42"`
	// Class is action, disruption or info. Required unless resolve is true.
	Class Class `json:"class" example:"action" enums:"action,disruption,info"`
	// Resolve closes the open event with this key: nothing more is delivered or reminded of it.
	Resolve bool `json:"resolve"`
	// Recipients are people (user = mail address) and groups of the role provider. Required unless resolve is true.
	Recipients []Recipient `json:"recipients"`
	// Actor is the person whose action caused the event; they are never notified about it.
	Actor string `json:"actor,omitempty" example:"someone@example.edu"`
	// Due is a deadline (RFC 3339). An action due within 48 hours is delivered during quiet hours too.
	Due *time.Time `json:"due,omitempty" example:"2026-12-09T00:00:00Z"`
	// Object is what the event is about.
	Object Object `json:"object"`
	// Data is free-form input for the texts.
	Data map[string]any `json:"data,omitempty" swaggertype:"object"`
}

var typePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]{0,99}$`)

// Validate checks r and normalises its addresses (trimmed, lowercased). The error names the first problem.
func (r *EventRequest) Validate() error {
	r.Source = strings.TrimSpace(r.Source)
	r.Type = strings.TrimSpace(r.Type)
	r.Key = strings.TrimSpace(r.Key)
	if !typePattern.MatchString(r.Type) {
		return fmt.Errorf("type must be lowercase letters, digits, '.', '_' or '-' (at most 100)")
	}
	if r.Key == "" || len(r.Key) > 300 {
		return fmt.Errorf("key is required (at most 300 characters)")
	}
	if len(r.Source) > 100 {
		return fmt.Errorf("source is too long (at most 100 characters)")
	}
	if r.Resolve {
		// A resolve needs only the key; class and recipients may be sent along and are ignored.
		return nil
	}
	if !r.Class.Valid() {
		return fmt.Errorf("class must be action, disruption or info")
	}
	if len(r.Recipients) == 0 {
		return fmt.Errorf("recipients must name at least one user or group")
	}
	if len(r.Recipients) > 1000 {
		return fmt.Errorf("at most 1000 recipients")
	}
	for i := range r.Recipients {
		rc := &r.Recipients[i]
		rc.User, rc.Group = strings.TrimSpace(rc.User), strings.TrimSpace(rc.Group)
		switch {
		case rc.User != "" && rc.Group != "":
			return fmt.Errorf("recipients[%d]: either user or group, not both", i)
		case rc.User != "":
			addr, err := NormalizeAddress(rc.User)
			if err != nil {
				return fmt.Errorf("recipients[%d].user: %v", i, err)
			}
			rc.User = addr
		case rc.Group != "":
			if !strings.HasPrefix(rc.Group, "group:") || len(rc.Group) <= len("group:") {
				return fmt.Errorf("recipients[%d].group must be a role-provider userset, group:<id> or group:<id>#<relation>", i)
			}
		default:
			return fmt.Errorf("recipients[%d] names neither user nor group", i)
		}
	}
	if r.Actor != "" {
		addr, err := NormalizeAddress(r.Actor)
		if err != nil {
			return fmt.Errorf("actor: %v", err)
		}
		r.Actor = addr
	}
	if r.Object.URL != "" {
		// Links go into HTML mails: only http(s), never javascript: or data:.
		u, err := url.Parse(r.Object.URL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return fmt.Errorf("object.url must be an absolute http(s) URL")
		}
	}
	return nil
}

// NormalizeAddress checks that s is a plain mail address and returns it trimmed and lowercased.
func NormalizeAddress(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	a, err := mail.ParseAddress(s)
	if err != nil || a.Address != s || a.Name != "" {
		return "", fmt.Errorf("%q is not a mail address", s)
	}
	return s, nil
}

// Mode is how a class reaches a person.
type Mode string

const (
	ModeImmediate Mode = "immediate"
	ModeDigest    Mode = "digest"
	ModeOff       Mode = "off"
)

// Digest is how often the summary comes.
type Digest string

const (
	DigestDaily  Digest = "daily"
	DigestWeekly Digest = "weekly"
)

// QuietHours hold back immediate mails: every night from Start to End (local time, "HH:MM") and, with Weekends, all of Saturday and Sunday. An action due within 48 hours is delivered anyway.
type QuietHours struct {
	Enabled  bool   `json:"enabled"`
	Start    string `json:"start" example:"20:00"`
	End      string `json:"end" example:"07:00"`
	Weekends bool   `json:"weekends"`
}

// Preferences are one person's settings.
type Preferences struct {
	// Language of the mails, de or en.
	Language string `json:"language" example:"de" enums:"de,en"`
	// Classes say per class whether it comes at once, in the summary or not at all. action: immediate or digest; disruption: immediate, digest or off; info: digest or off.
	Classes map[Class]Mode `json:"classes"`
	// Digest is how often the summary comes: daily or weekly (Mondays).
	Digest     Digest     `json:"digest" example:"weekly" enums:"daily,weekly"`
	QuietHours QuietHours `json:"quiet_hours"`
	// Unsubscribed lists the event types the person gets no mail about.
	Unsubscribed []string `json:"unsubscribed"`
}

// DefaultPreferences are what applies to somebody who has saved none: action and disruption at once, info in a weekly summary, quiet at night and on weekends.
func DefaultPreferences() Preferences {
	return Preferences{
		Language:     "de",
		Classes:      map[Class]Mode{ClassAction: ModeImmediate, ClassDisruption: ModeImmediate, ClassInfo: ModeDigest},
		Digest:       DigestWeekly,
		QuietHours:   QuietHours{Enabled: true, Start: "20:00", End: "07:00", Weekends: true},
		Unsubscribed: []string{},
	}
}

// allowedModes are the modes each class may be set to: an action cannot be switched off (only collected), an info never comes at once.
var allowedModes = map[Class][]Mode{
	ClassAction:     {ModeImmediate, ModeDigest},
	ClassDisruption: {ModeImmediate, ModeDigest, ModeOff},
	ClassInfo:       {ModeDigest, ModeOff},
}

// AllowedModes are the modes class c may be set to.
func AllowedModes(c Class) []Mode { return allowedModes[c] }

// Validate checks p and fills what is missing from the defaults.
func (p *Preferences) Validate() error {
	def := DefaultPreferences()
	if p.Language == "" {
		p.Language = def.Language
	}
	if p.Language != "de" && p.Language != "en" {
		return fmt.Errorf("language must be de or en")
	}
	if p.Classes == nil {
		p.Classes = map[Class]Mode{}
	}
	for c := range p.Classes {
		if !c.Valid() {
			return fmt.Errorf("unknown class %q", c)
		}
	}
	for _, c := range Classes {
		m, ok := p.Classes[c]
		if !ok {
			p.Classes[c] = def.Classes[c]
			continue
		}
		if !slices.Contains(allowedModes[c], m) {
			return fmt.Errorf("class %s cannot be %q (allowed: %v)", c, m, allowedModes[c])
		}
	}
	if p.Digest == "" {
		p.Digest = def.Digest
	}
	if p.Digest != DigestDaily && p.Digest != DigestWeekly {
		return fmt.Errorf("digest must be daily or weekly")
	}
	if p.QuietHours.Start == "" {
		p.QuietHours.Start = def.QuietHours.Start
	}
	if p.QuietHours.End == "" {
		p.QuietHours.End = def.QuietHours.End
	}
	if _, err := ParseClock(p.QuietHours.Start); err != nil {
		return fmt.Errorf("quiet_hours.start: %v", err)
	}
	if _, err := ParseClock(p.QuietHours.End); err != nil {
		return fmt.Errorf("quiet_hours.end: %v", err)
	}
	if p.Unsubscribed == nil {
		p.Unsubscribed = []string{}
	}
	seen := map[string]bool{}
	out := p.Unsubscribed[:0]
	for _, t := range p.Unsubscribed {
		t = strings.TrimSpace(t)
		if !typePattern.MatchString(t) {
			return fmt.Errorf("unsubscribed: %q is not an event type", t)
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	p.Unsubscribed = out
	return nil
}

// ModeOf is the mode for class c.
func (p Preferences) ModeOf(c Class) Mode {
	if m, ok := p.Classes[c]; ok {
		return m
	}
	return DefaultPreferences().Classes[c]
}

// IsUnsubscribed says whether the person gets no mail about events of type t.
func (p Preferences) IsUnsubscribed(t string) bool { return slices.Contains(p.Unsubscribed, t) }

// ParseClock reads "HH:MM" as minutes after midnight.
func ParseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("%q is not HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}
