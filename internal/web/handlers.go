package web

import (
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/pfisterer/cloud-self-service-golib/ginweb"
	"github.com/pfisterer/cloud-self-service-golib/msg"
	"github.com/pfisterer/notifications/internal/model"
	"github.com/pfisterer/notifications/internal/texts"
	"github.com/pfisterer/notifications/internal/unsub"
)

type handlers struct {
	cfg Config
}

// EventResponse is the answer to POST /v1/events.
type EventResponse struct {
	// Status is accepted, duplicate (an open event with this key exists; nothing new is delivered), resolved or nothing_to_resolve.
	Status string `json:"status" example:"accepted"`
}

// ErrorResponse is the body of every error.
type ErrorResponse struct {
	Error string `json:"error"`
}

// postEvent godoc
//
//	@Summary		Post an event
//	@Description	A service reports an event; this service decides whether, when and how it reaches people. Answers 202 for every valid event, a duplicate (an open event with the same key from the same service token) included, and 400 for an invalid body. With resolve=true the open event with this key is closed: what of it has not gone out yet is dropped and no reminder follows. Groups are resolved to people through the role provider; the actor never gets a notification about their own action.
//	@Tags			events
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			event	body		model.EventRequest	true	"The event"
//	@Success		202		{object}	EventResponse
//	@Failure		400		{object}	ErrorResponse
//	@Failure		401		{object}	ErrorResponse
//	@Router			/v1/events [post]
func (h *handlers) postEvent(c *gin.Context) {
	var req model.EventRequest
	dec := json.NewDecoder(c.Request.Body)
	if err := dec.Decode(&req); err != nil {
		abort(c, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := req.Validate(); err != nil {
		abort(c, http.StatusBadRequest, err.Error())
		return
	}
	out, err := h.cfg.Engine.Ingest(c.Request.Context(), c.GetString(senderKey), req)
	if err != nil {
		h.cfg.Log.Errorw("storing an event failed", "error", err)
		abort(c, http.StatusInternalServerError, "the event could not be stored")
		return
	}
	c.JSON(http.StatusAccepted, EventResponse{Status: string(out)})
}

// PreferencesResponse is a person's settings as they apply.
type PreferencesResponse struct {
	model.Preferences
	// Recipient is the address the settings belong to.
	Recipient string `json:"recipient"`
	// IsDefault says the person has saved nothing and the defaults apply.
	IsDefault bool `json:"is_default"`
}

// getPreferences godoc
//
//	@Summary		Own notification settings
//	@Description	The settings of the person signed in, or the defaults when they have saved none (action and disruption at once, info in a weekly summary).
//	@Tags			preferences
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	PreferencesResponse
//	@Failure		401	{object}	ErrorResponse
//	@Router			/v1/preferences [get]
func (h *handlers) getPreferences(c *gin.Context) {
	who := c.GetString(personKey)
	p, _, ok, err := h.cfg.Store.Preferences(c.Request.Context(), who)
	if err != nil {
		abort(c, http.StatusInternalServerError, "settings could not be read")
		return
	}
	c.JSON(http.StatusOK, PreferencesResponse{Preferences: p, Recipient: who, IsDefault: !ok})
}

// putPreferences godoc
//
//	@Summary		Save own notification settings
//	@Description	Replaces the settings of the person signed in. Missing fields take the defaults. action can be immediate or digest, disruption immediate, digest or off, info digest or off.
//	@Tags			preferences
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			preferences	body		model.Preferences	true	"Settings"
//	@Success		200			{object}	PreferencesResponse
//	@Failure		400			{object}	ErrorResponse
//	@Failure		401			{object}	ErrorResponse
//	@Router			/v1/preferences [put]
func (h *handlers) putPreferences(c *gin.Context) {
	who := c.GetString(personKey)
	var p model.Preferences
	if err := json.NewDecoder(c.Request.Body).Decode(&p); err != nil {
		abort(c, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if err := p.Validate(); err != nil {
		abort(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.cfg.Store.SavePreferences(c.Request.Context(), who, p, time.Now().UTC()); err != nil {
		abort(c, http.StatusInternalServerError, "settings could not be saved")
		return
	}
	c.JSON(http.StatusOK, PreferencesResponse{Preferences: p, Recipient: who})
}

// TypeInfo is one event type with its own text.
type TypeInfo struct {
	Type string `json:"type" example:"approval.waiting"`
	// Class is the class senders use for it.
	Class string `json:"class" example:"action"`
	// Name in the language of the request (Accept-Language).
	Name string `json:"name" example:"Antrag wartet auf Entscheidung"`
}

// TypesResponse lists the event types and the modes each class may be set to.
type TypesResponse struct {
	Types []TypeInfo `json:"types"`
	// Modes are the modes each class may be set to.
	Modes map[string][]string `json:"modes"`
}

// getTypes godoc
//
//	@Summary		Event types
//	@Description	The event types with a text of their own, for the settings page (unsubscribing per type), and the modes each class may be set to. Names in the language of Accept-Language.
//	@Tags			preferences
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	TypesResponse
//	@Router			/v1/types [get]
func (h *handlers) getTypes(c *gin.Context) {
	l := langOf(c)
	out := TypesResponse{Modes: map[string][]string{}}
	for _, k := range texts.Known() {
		out.Types = append(out.Types, TypeInfo{Type: k.Type, Class: string(k.Class), Name: k.Name(l)})
	}
	for _, cl := range model.Classes {
		for _, m := range model.AllowedModes(cl) {
			out.Modes[string(cl)] = append(out.Modes[string(cl)], string(m))
		}
	}
	c.JSON(http.StatusOK, out)
}

// NotificationView is one notification as the person sees it.
type NotificationView struct {
	ID     uint64       `json:"id"`
	Type   string       `json:"type"`
	Class  string       `json:"class"`
	Title  string       `json:"title"`
	Text   string       `json:"text"`
	URL    string       `json:"url,omitempty"`
	Object model.Object `json:"object"`
	// Status is pending, digest (waits for the summary), sent, digested or failed.
	Status    string     `json:"status"`
	Reminder  int        `json:"reminder"`
	CreatedAt time.Time  `json:"created_at"`
	SentAt    *time.Time `json:"sent_at,omitempty"`
	// Open says the event has not been resolved yet.
	Open bool `json:"open"`
}

// getNotifications godoc
//
//	@Summary		Own recent notifications
//	@Description	The latest notifications of the person signed in, newest first, rendered in the language of Accept-Language. Suppressed ones (own actions, unsubscribed types, resolved before delivery) are not listed.
//	@Tags			notifications
//	@Produce		json
//	@Security		Bearer
//	@Param			limit	query		int	false	"At most this many (default 50, at most 200)"
//	@Success		200		{array}		NotificationView
//	@Failure		401		{object}	ErrorResponse
//	@Router			/v1/notifications [get]
func (h *handlers) getNotifications(c *gin.Context) {
	limit := 50
	if s := c.Query("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 {
			abort(c, http.StatusBadRequest, "limit must be a positive number")
			return
		}
		limit = min(n, 200)
	}
	ns, err := h.cfg.Store.Recent(c.Request.Context(), c.GetString(personKey), limit)
	if err != nil {
		abort(c, http.StatusInternalServerError, "notifications could not be read")
		return
	}
	l := langOf(c)
	out := make([]NotificationView, 0, len(ns))
	for _, n := range ns {
		v := texts.View{Type: n.Type, Reminder: n.Reminder, Loc: h.cfg.Engine.Policy.Location}
		open := true
		if n.Event != nil {
			v.Object, v.Data, v.Due = h.cfg.Engine.Links.Complete(n.Event.ObjectValue()), n.Event.DataValue(), n.Event.Due
			open = n.Event.ResolvedAt == nil
		}
		it := texts.Render(v, l)
		out = append(out, NotificationView{ID: n.ID, Type: n.Type, Class: n.Class, Title: it.Title, Text: it.Body, URL: it.URL, Object: v.Object, Status: n.Status, Reminder: n.Reminder, CreatedAt: n.CreatedAt, SentAt: n.SentAt, Open: open})
	}
	c.JSON(http.StatusOK, out)
}

func langOf(c *gin.Context) texts.Lang {
	if ginweb.Lang(c) == msg.EN {
		return texts.EN
	}
	return texts.DE
}

// adminPass godoc
//
//	@Summary		Run a delivery pass now
//	@Description	Fans out new events, queues due reminders and delivers what is due, without waiting for the next tick. The rules (bundling, limits) apply as always.
//	@Tags			admin
//	@Produce		json
//	@Security		Bearer
//	@Success		200	{object}	map[string]string
//	@Failure		401	{object}	ErrorResponse
//	@Router			/v1/admin/pass [post]
func (h *handlers) adminPass(c *gin.Context) {
	if err := h.cfg.Engine.Pass(c.Request.Context()); err != nil {
		abort(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "done"})
}

// DigestRequest names whose summary to send; empty: everybody's.
type DigestRequest struct {
	Recipient string `json:"recipient,omitempty"`
}

// adminDigests godoc
//
//	@Summary		Send summaries now
//	@Description	Sends the summary of one person (recipient) or of everybody with something waiting, regardless of its schedule. For tests and for catching up.
//	@Tags			admin
//	@Accept			json
//	@Produce		json
//	@Security		Bearer
//	@Param			request	body		DigestRequest	false	"Whose summary"
//	@Success		200		{object}	map[string]int
//	@Failure		401		{object}	ErrorResponse
//	@Router			/v1/admin/digests [post]
func (h *handlers) adminDigests(c *gin.Context) {
	var req DigestRequest
	if err := json.NewDecoder(c.Request.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		abort(c, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	who := strings.ToLower(strings.TrimSpace(req.Recipient))
	// Fan out and route first, so what was just posted is in the summary.
	if err := h.cfg.Engine.Pass(c.Request.Context()); err != nil {
		h.cfg.Log.Warnw("pass before the summaries failed", "error", err)
	}
	n, err := h.cfg.Engine.SendDigests(c.Request.Context(), who)
	if err != nil {
		abort(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"sent": n})
}

// ── Unsubscribe ──────────────────────────────────────────────────────────────

var unsubTmpl = template.Must(template.New("u").Parse(`<!DOCTYPE html>
<html lang="{{.Lang}}"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Title}}</title>
<style>body{font:15px/1.5 Arial,sans-serif;margin:3rem auto;max-width:36rem;padding:0 1rem;color:#1a1e20}button{background:#d52c2a;color:#fff;border:0;border-radius:4px;padding:8px 16px;font-size:15px;cursor:pointer}a{color:#991b1a}</style>
</head><body><h1 style="font-size:20px">{{.Title}}</h1><p>{{.Text}}</p>
{{if .Token}}<form method="post" action="unsubscribe"><input type="hidden" name="token" value="{{.Token}}"><button type="submit">{{.Button}}</button></form>{{end}}
{{if .Settings}}<p style="margin-top:2rem;font-size:13px"><a href="{{.Settings}}">{{.SettingsLabel}}</a></p>{{end}}
</body></html>`))

func (h *handlers) renderUnsub(c *gin.Context, status int, data map[string]any) {
	l := langOf(c)
	data["Lang"] = string(l)
	data["Settings"] = h.cfg.Engine.Links.SettingsURL
	data["SettingsLabel"] = tr(l, "Alle Einstellungen für Benachrichtigungen (Anmeldung nötig)", "All notification settings (sign-in required)")
	// The page has a form and inline styles and nothing else.
	c.Header("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'")
	c.Header("Cache-Control", "no-store")
	c.Status(status)
	c.Header("Content-Type", "text/html; charset=utf-8")
	_ = unsubTmpl.Execute(c.Writer, data)
}

func tr(l texts.Lang, de, en string) string {
	if l == texts.EN {
		return en
	}
	return de
}

func describe(claim unsub.Claim, l texts.Lang) string {
	if claim.Type == unsub.InfoClass {
		return tr(l, "Zusammenfassungen mit Infos", "summaries of information")
	}
	return tr(l, "Benachrichtigungen der Art „", "notifications of the kind “") + texts.KindOf(claim.Type).Name(l) + tr(l, "“", "”")
}

// unsubscribePage shows what the link would end and a button for it. A GET never changes anything: mail scanners follow links.
func (h *handlers) unsubscribePage(c *gin.Context) {
	l := langOf(c)
	token := c.Query("token")
	claim, err := h.cfg.Engine.Signer.Verify(token)
	if err != nil {
		h.renderUnsub(c, http.StatusBadRequest, map[string]any{"Title": tr(l, "Link ungültig", "Invalid link"), "Text": tr(l, "Dieser Abmelde-Link ist ungültig oder unvollständig.", "This unsubscribe link is invalid or incomplete.")})
		return
	}
	h.renderUnsub(c, http.StatusOK, map[string]any{
		"Title":  tr(l, "Benachrichtigungen abbestellen", "Unsubscribe"),
		"Text":   tr(l, "Für ", "Stop ") + describe(claim, l) + tr(l, " an "+claim.Recipient+" keine Mails mehr schicken?", " to "+claim.Recipient+"?"),
		"Token":  token,
		"Button": tr(l, "Abbestellen", "Unsubscribe"),
	})
}

// unsubscribe ends the mails the token names: from the page's button, or as RFC 8058 one-click POST from a mail program (body List-Unsubscribe=One-Click, token in the URL).
func (h *handlers) unsubscribe(c *gin.Context) {
	l := langOf(c)
	token := c.Query("token")
	if token == "" {
		token = c.PostForm("token")
	}
	claim, err := h.cfg.Engine.Signer.Verify(token)
	if err != nil {
		h.renderUnsub(c, http.StatusBadRequest, map[string]any{"Title": tr(l, "Link ungültig", "Invalid link"), "Text": tr(l, "Dieser Abmelde-Link ist ungültig oder unvollständig.", "This unsubscribe link is invalid or incomplete.")})
		return
	}
	ctx := c.Request.Context()
	p, _, _, err := h.cfg.Store.Preferences(ctx, claim.Recipient)
	if err == nil {
		if claim.Type == unsub.InfoClass {
			p.Classes[model.ClassInfo] = model.ModeOff
		} else if !p.IsUnsubscribed(claim.Type) {
			p.Unsubscribed = append(p.Unsubscribed, claim.Type)
		}
		err = h.cfg.Store.SavePreferences(ctx, claim.Recipient, p, time.Now().UTC())
	}
	if err != nil {
		h.cfg.Log.Errorw("unsubscribe failed", "error", err)
		h.renderUnsub(c, http.StatusInternalServerError, map[string]any{"Title": tr(l, "Fehler", "Error"), "Text": tr(l, "Die Abmeldung konnte nicht gespeichert werden. Bitte später erneut versuchen.", "The change could not be saved. Please try again later.")})
		return
	}
	h.cfg.Log.Infow("unsubscribed by link", "what", claim.Type)
	h.renderUnsub(c, http.StatusOK, map[string]any{
		"Title": tr(l, "Abbestellt", "Unsubscribed"),
		"Text":  tr(l, "Für ", "No more mail about ") + describe(claim, l) + tr(l, " kommen keine Mails mehr an "+claim.Recipient+". Rückgängig machen lässt sich das in den Einstellungen im Portal.", " will be sent to "+claim.Recipient+". This can be undone in the settings in the portal."),
	})
}
