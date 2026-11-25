package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/notifications/internal/engine"
	"github.com/pfisterer/notifications/internal/mail"
	"github.com/pfisterer/notifications/internal/model"
	"github.com/pfisterer/notifications/internal/roleprovider"
	"github.com/pfisterer/notifications/internal/store"
	"github.com/pfisterer/notifications/internal/unsub"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

const (
	svcToken   = "service-token-of-os-mgt-at-least-32-chars"
	adminToken = "admin-token-for-tests-at-least-32-characters"
	person     = "carol@example.edu"
)

type env struct {
	t   *testing.T
	r   *gin.Engine
	e   *engine.Engine
	st  *store.Store
	rec *mail.Recorder
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dsn := "file:" + strings.ReplaceAll(t.Name(), "/", "_") + "?mode=memory&cache=shared"
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
	signer, _ := unsub.New(strings.Repeat("k", 40))
	rec := &mail.Recorder{}
	e := &engine.Engine{Store: st, Deliverer: rec, Resolver: roleprovider.Static{}, Signer: signer, Policy: engine.DefaultPolicy(), Links: engine.Links{PublicURL: "https://notify.example.edu", SettingsURL: "https://portal.example.edu/settings"}}
	r, err := SetupRouter(Config{
		DevMode: true, Log: zap.NewNop().Sugar(), Engine: e, Store: st,
		ServiceTokens: []ServiceToken{{Name: "openstack-management-api", Token: svcToken}},
		AdminTokens:   []string{adminToken},
		DevAuthUser:   person,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, r: r, e: e, st: st, rec: rec}
}

func (v *env) do(method, path, token, body string, header ...string) *httptest.ResponseRecorder {
	v.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	w := httptest.NewRecorder()
	v.r.ServeHTTP(w, req)
	return w
}

const validEvent = `{"source":"openstack-management-api","type":"allocation.expired","key":"allocation-expired:a1","class":"action","resolve":false,
 "recipients":[{"user":"Dana@Example.edu"},{"group":"group:dualis-ma-wdski25a#studierende"}],
 "actor":"carol@example.edu","due":"2026-12-09T00:00:00Z",
 "object":{"kind":"project","id":"p_1","name":"Kursprojekt","url":"https://self-service.staging.dhbw.cloud/cloud/projects/p_1"},
 "data":{"budget":"DSKI","shrink_at":"2026-12-09"}}`

func TestPostEvent(t *testing.T) {
	v := newEnv(t)
	w := v.do("POST", "/v1/events", svcToken, validEvent)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"accepted"`) {
		t.Fatalf("first: %d %s", w.Code, w.Body)
	}
	// The duplicate is answered 202 too, and says so.
	w = v.do("POST", "/v1/events", svcToken, validEvent)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"duplicate"`) {
		t.Fatalf("duplicate: %d %s", w.Code, w.Body)
	}
	var evs []store.Event
	v.st.DB().Find(&evs)
	if len(evs) != 1 || evs[0].Sender != "openstack-management-api" || !strings.Contains(evs[0].Recipients, "dana@example.edu") {
		t.Fatalf("stored: %+v", evs)
	}
	w = v.do("POST", "/v1/events", svcToken, `{"type":"allocation.expired","key":"allocation-expired:a1","resolve":true}`)
	if w.Code != http.StatusAccepted || !strings.Contains(w.Body.String(), `"resolved"`) {
		t.Fatalf("resolve: %d %s", w.Code, w.Body)
	}
}

func TestPostEventRefusals(t *testing.T) {
	v := newEnv(t)
	if w := v.do("POST", "/v1/events", "", validEvent); w.Code != http.StatusUnauthorized {
		t.Errorf("no token: %d", w.Code)
	}
	if w := v.do("POST", "/v1/events", "wrong-token-wrong-token-wrong-token", validEvent); w.Code != http.StatusUnauthorized {
		t.Errorf("wrong token: %d", w.Code)
	}
	if w := v.do("POST", "/v1/events", adminToken, validEvent); w.Code != http.StatusUnauthorized {
		t.Errorf("admin token on events: %d", w.Code)
	}
	bad := map[string]string{
		"not json":         `{`,
		"no type":          `{"key":"k","class":"info","recipients":[{"user":"a@example.edu"}]}`,
		"no key":           `{"type":"x.y","class":"info","recipients":[{"user":"a@example.edu"}]}`,
		"bad class":        `{"type":"x.y","key":"k","class":"urgent","recipients":[{"user":"a@example.edu"}]}`,
		"no recipients":    `{"type":"x.y","key":"k","class":"info","recipients":[]}`,
		"bad user":         `{"type":"x.y","key":"k","class":"info","recipients":[{"user":"not an address"}]}`,
		"user and group":   `{"type":"x.y","key":"k","class":"info","recipients":[{"user":"a@example.edu","group":"group:g"}]}`,
		"bad group":        `{"type":"x.y","key":"k","class":"info","recipients":[{"group":"g"}]}`,
		"javascript link":  `{"type":"x.y","key":"k","class":"info","recipients":[{"user":"a@example.edu"}],"object":{"url":"javascript:alert(1)"}}`,
		"bad due":          `{"type":"x.y","key":"k","class":"info","recipients":[{"user":"a@example.edu"}],"due":"tomorrow"}`,
		"type with spaces": `{"type":"Approval Waiting","key":"k","class":"info","recipients":[{"user":"a@example.edu"}]}`,
	}
	for name, body := range bad {
		if w := v.do("POST", "/v1/events", svcToken, body); w.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, w.Code, w.Body)
		}
	}
}

func TestUnknownTypeGetsTheGenericText(t *testing.T) {
	v := newEnv(t)
	now := time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)
	v.e.Now = func() time.Time { return now }
	body := `{"type":"zone.deleted","key":"z1","class":"disruption","recipients":[{"user":"dana@example.edu"}],"data":{"title":{"de":"Zone gelöscht","en":"Zone deleted"},"text":"Die Zone example.dhbw.cloud wurde gelöscht."}}`
	if w := v.do("POST", "/v1/events", svcToken, body); w.Code != http.StatusAccepted {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	now = now.Add(5 * time.Minute)
	if w := v.do("POST", "/v1/admin/pass", adminToken, ""); w.Code != http.StatusOK {
		t.Fatalf("pass: %d %s", w.Code, w.Body)
	}
	ms := v.rec.Messages()
	if len(ms) != 1 || ms[0].Subject != "Zone gelöscht" || !strings.Contains(ms[0].Text, "example.dhbw.cloud wurde gelöscht") {
		t.Fatalf("mails: %+v", ms)
	}
}

func TestAdminDigests(t *testing.T) {
	v := newEnv(t)
	body := `{"type":"approval.decided","key":"d1","class":"info","recipients":[{"user":"dana@example.edu"}],"data":{"decision":"rejected"}}`
	v.do("POST", "/v1/events", svcToken, body)
	if w := v.do("POST", "/v1/admin/digests", "", `{}`); w.Code != http.StatusUnauthorized {
		t.Errorf("without token: %d", w.Code)
	}
	w := v.do("POST", "/v1/admin/digests", adminToken, `{"recipient":"dana@example.edu"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"sent":1`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if ms := v.rec.Messages(); len(ms) != 1 || !strings.Contains(ms[0].Text, "Antrag abgelehnt") {
		t.Fatalf("mails: %+v", ms)
	}
}

func TestPreferences(t *testing.T) {
	v := newEnv(t)
	w := v.do("GET", "/v1/preferences", "", "")
	if w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var got PreferencesResponse
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if !got.IsDefault || got.Recipient != person || got.ModeOf(model.ClassInfo) != model.ModeDigest || got.Digest != model.DigestWeekly {
		t.Fatalf("defaults: %+v", got)
	}
	w = v.do("PUT", "/v1/preferences", "", `{"language":"en","classes":{"info":"off"},"digest":"daily","quiet_hours":{"enabled":false},"unsubscribed":["project.ending"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("put: %d %s", w.Code, w.Body)
	}
	w = v.do("GET", "/v1/preferences", "", "")
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got.IsDefault || got.Language != "en" || got.ModeOf(model.ClassInfo) != model.ModeOff || got.ModeOf(model.ClassAction) != model.ModeImmediate || got.QuietHours.Enabled || got.QuietHours.Start != "20:00" {
		t.Fatalf("saved: %+v", got)
	}
	if w := v.do("PUT", "/v1/preferences", "", `{"classes":{"info":"immediate"}}`); w.Code != http.StatusBadRequest {
		t.Errorf("info immediate: %d", w.Code)
	}
	if w := v.do("PUT", "/v1/preferences", "", `{"classes":{"action":"off"}}`); w.Code != http.StatusBadRequest {
		t.Errorf("action off: %d", w.Code)
	}
	if w := v.do("GET", "/v1/preferences", "", "", "X-Act-As", "someone@example.edu"); w.Code != http.StatusForbidden {
		t.Errorf("act-as: %d", w.Code)
	}
	w = v.do("GET", "/v1/types", "", "", "Accept-Language", "en")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"Allocation expired"`) || !strings.Contains(w.Body.String(), `"modes"`) {
		t.Errorf("types: %d %s", w.Code, w.Body)
	}
}

func TestRecentNotifications(t *testing.T) {
	v := newEnv(t)
	body := `{"type":"approval.waiting","key":"n1","class":"action","recipients":[{"user":"carol@example.edu"}],"object":{"name":"Projekt X"}}`
	v.do("POST", "/v1/events", svcToken, body)
	if err := v.e.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	w := v.do("GET", "/v1/notifications", "", "", "Accept-Language", "en")
	var got []NotificationView
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusOK || len(got) != 1 || got[0].Title != "Request awaiting a decision: Projekt X" || !got[0].Open {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
}

func TestUnsubscribeLink(t *testing.T) {
	v := newEnv(t)
	token := v.e.Signer.Sign(unsub.Claim{Recipient: "dana@example.edu", Type: "project.ending"})
	// Showing the page changes nothing.
	w := v.do("GET", "/unsubscribe?token="+url.QueryEscape(token), "", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "<form") {
		t.Fatalf("page: %d %s", w.Code, w.Body)
	}
	if _, _, ok, _ := v.st.Preferences(context.Background(), "dana@example.edu"); ok {
		t.Fatal("GET must not change anything")
	}
	// RFC 8058 one-click POST from a mail program.
	req := httptest.NewRequest("POST", "/unsubscribe?token="+url.QueryEscape(token), bytes.NewBufferString("List-Unsubscribe=One-Click"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw := httptest.NewRecorder()
	v.r.ServeHTTP(rw, req)
	if rw.Code != http.StatusOK {
		t.Fatalf("post: %d %s", rw.Code, rw.Body)
	}
	p, _, ok, _ := v.st.Preferences(context.Background(), "dana@example.edu")
	if !ok || !p.IsUnsubscribed("project.ending") {
		t.Fatalf("not unsubscribed: %+v", p)
	}
	// The summary's link switches the info class off.
	info := v.e.Signer.Sign(unsub.Claim{Recipient: "dana@example.edu", Type: unsub.InfoClass})
	form := url.Values{"token": {info}}
	req = httptest.NewRequest("POST", "/unsubscribe", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rw = httptest.NewRecorder()
	v.r.ServeHTTP(rw, req)
	p, _, _, _ = v.st.Preferences(context.Background(), "dana@example.edu")
	if rw.Code != http.StatusOK || p.ModeOf(model.ClassInfo) != model.ModeOff || !p.IsUnsubscribed("project.ending") {
		t.Fatalf("info off: %d %+v", rw.Code, p)
	}
	// A forged token is refused.
	if w := v.do("GET", "/unsubscribe?token="+url.QueryEscape(token+"x"), "", ""); w.Code != http.StatusBadRequest {
		t.Errorf("forged: %d", w.Code)
	}
}
