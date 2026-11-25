package texts

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
)

// Content is a mail's subject and its two bodies.
type Content struct {
	Subject string
	Text    string
	HTML    string
}

// Footer are the links every mail ends with.
type Footer struct {
	// SettingsURL is the portal page with the person's notification settings.
	SettingsURL string
	// UnsubscribeURL ends mails like this one (one event type, or the summary's info class) without signing in.
	UnsubscribeURL string
	// UnsubscribeWhat names what UnsubscribeURL ends, in the mail's language.
	UnsubscribeWhat string
}

// Section is a heading with items (one per event type in a summary).
type Section struct {
	Heading string
	Items   []Item
}

// Immediate builds the mail for items of one event type typ: a single item is its own subject, several are bundled under the type's bundle subject.
func Immediate(l Lang, typ string, items []Item, reminders bool, f Footer) Content {
	k := KindOf(typ)
	subject := ""
	if len(items) == 1 {
		subject = items[0].Title
	} else {
		subject = k.Bundle(len(items), l)
		if reminders {
			subject = tr(l, "Erinnerung: ", "Reminder: ") + subject
		}
	}
	intro := ""
	if len(items) > 1 {
		intro = tr(l, "Mehrere Ereignisse derselben Art, in einer Nachricht zusammengefasst:", "Several events of the same kind, collected in one message:")
	}
	return build(l, subject, intro, []Section{{Items: items}}, f)
}

// Digest builds the summary from sections (one per event type).
func Digest(l Lang, sections []Section, f Footer) Content {
	n := 0
	for _, s := range sections {
		n += len(s.Items)
	}
	subject := tr(l, fmt.Sprintf("Zusammenfassung: %d Benachrichtigungen", n), fmt.Sprintf("Summary: %d notifications", n))
	if n == 1 {
		subject = tr(l, "Zusammenfassung: 1 Benachrichtigung", "Summary: 1 notification")
	}
	intro := tr(l, "Was seit der letzten Zusammenfassung in der DHBW Cloud passiert ist:", "What happened in the DHBW Cloud since the last summary:")
	return build(l, subject, intro, sections, f)
}

func build(l Lang, subject, intro string, sections []Section, f Footer) Content {
	var tb strings.Builder
	if intro != "" {
		tb.WriteString(intro + "\n\n")
	}
	for _, s := range sections {
		if s.Heading != "" {
			tb.WriteString("== " + s.Heading + " ==\n\n")
		}
		for _, it := range s.Items {
			tb.WriteString("* " + it.Title + "\n")
			if it.Body != "" {
				tb.WriteString("  " + strings.ReplaceAll(it.Body, "\n", "\n  ") + "\n")
			}
			if it.URL != "" {
				tb.WriteString("  " + it.Link + ": " + it.URL + "\n")
			}
			tb.WriteString("\n")
		}
	}
	tb.WriteString("-- \n")
	tb.WriteString(footerLead(l) + "\n")
	if f.SettingsURL != "" {
		tb.WriteString(tr(l, "Einstellungen: ", "Settings: ") + f.SettingsURL + "\n")
	}
	if f.UnsubscribeURL != "" {
		tb.WriteString(tr(l, "Abbestellen (", "Unsubscribe (") + f.UnsubscribeWhat + "): " + f.UnsubscribeURL + "\n")
	}

	var hb bytes.Buffer
	_ = htmlTmpl.Execute(&hb, map[string]any{
		"Lang": string(l), "Subject": subject, "Intro": intro, "Sections": sections, "Footer": f,
		"Lead": footerLead(l), "SettingsLabel": tr(l, "Einstellungen für Benachrichtigungen", "Notification settings"),
		"UnsubLabel": tr(l, "Abbestellen: ", "Unsubscribe: ") + f.UnsubscribeWhat,
	})
	return Content{Subject: subject, Text: tb.String(), HTML: hb.String()}
}

func footerLead(l Lang) string {
	return tr(l, "Diese Nachricht kommt automatisch vom Self-Service der DHBW Cloud.", "This message was sent automatically by the DHBW Cloud self-service.")
}

// paragraphs splits a body at blank lines for the HTML part.
func paragraphs(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

var htmlTmpl = template.Must(template.New("mail").Funcs(template.FuncMap{"paragraphs": paragraphs}).Parse(`<!DOCTYPE html>
<html lang="{{.Lang}}">
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1"><title>{{.Subject}}</title></head>
<body style="margin:0;padding:0;background:#f0f1f1;font-family:Arial,Helvetica,sans-serif;color:#1a1e20;">
<table role="presentation" width="100%" cellspacing="0" cellpadding="0" style="background:#f0f1f1;"><tr><td align="center" style="padding:24px 12px;">
<table role="presentation" width="600" cellspacing="0" cellpadding="0" style="max-width:600px;width:100%;background:#ffffff;border-radius:4px;">
<tr><td style="background:#d52c2a;color:#ffffff;padding:14px 24px;font-size:16px;font-weight:bold;border-radius:4px 4px 0 0;">DHBW Cloud</td></tr>
<tr><td style="padding:20px 24px 8px 24px;">
{{if .Intro}}<p style="margin:0 0 16px 0;font-size:14px;color:#474c4e;">{{.Intro}}</p>{{end}}
{{range .Sections}}
{{if .Heading}}<h2 style="margin:20px 0 8px 0;font-size:15px;color:#5f6466;text-transform:none;">{{.Heading}}</h2>{{end}}
{{range .Items}}
<div style="margin:0 0 18px 0;padding:0 0 14px 0;border-bottom:1px solid #d9dbdc;">
<p style="margin:0 0 6px 0;font-size:15px;font-weight:bold;">{{.Title}}</p>
{{range paragraphs .Body}}<p style="margin:0 0 8px 0;font-size:14px;line-height:1.45;">{{.}}</p>{{end}}
{{if .URL}}<p style="margin:8px 0 0 0;"><a href="{{.URL}}" style="display:inline-block;background:#d52c2a;color:#ffffff;text-decoration:none;padding:7px 14px;border-radius:4px;font-size:14px;">{{.Link}}</a></p>{{end}}
</div>
{{end}}
{{end}}
</td></tr>
<tr><td style="padding:8px 24px 20px 24px;font-size:12px;color:#788187;line-height:1.5;">
{{.Lead}}<br>
{{if .Footer.SettingsURL}}<a href="{{.Footer.SettingsURL}}" style="color:#788187;">{{.SettingsLabel}}</a>{{end}}
{{if .Footer.UnsubscribeURL}} · <a href="{{.Footer.UnsubscribeURL}}" style="color:#788187;">{{.UnsubLabel}}</a>{{end}}
</td></tr>
</table>
</td></tr></table>
</body>
</html>
`))
