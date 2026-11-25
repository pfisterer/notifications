package mail

import (
	"strings"
	"testing"
	"time"
)

func TestCompose(t *testing.T) {
	raw, err := Compose(SMTPConfig{From: "cloud@example.edu", FromName: "DHBW Cloud", ReplyTo: "service@example.edu"}, Message{
		To: "a@example.edu", Subject: "Antrag für Kurs", Text: "Text äöü", HTML: "<p>HTML</p>",
		Headers: map[string]string{"List-Unsubscribe": "<https://n.example.edu/unsubscribe?token=x>", "List-Unsubscribe-Post": "List-Unsubscribe=One-Click"},
	}, time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, want := range []string{"From: DHBW Cloud <cloud@example.edu>", "To: a@example.edu", "Reply-To: service@example.edu", "Subject: =?utf-8?q?Antrag_f=C3=BCr_Kurs?=", "Auto-Submitted: auto-generated", "List-Unsubscribe-Post: List-Unsubscribe=One-Click", "multipart/alternative", "text/html", "Text =C3=A4=C3=B6=C3=BC"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
	if _, err := Compose(SMTPConfig{From: "x@example.edu"}, Message{To: "a@example.edu\r\nBcc: b@example.edu"}, time.Now()); err == nil {
		t.Error("a line break in the address must be refused")
	}
}

func TestNewSMTPChecks(t *testing.T) {
	if _, err := NewSMTP(SMTPConfig{Host: "h", Port: 25, From: "a@example.edu", TLS: "none", Username: "u"}); err == nil {
		t.Error("login without TLS accepted")
	}
	if _, err := NewSMTP(SMTPConfig{Host: "h", Port: 25}); err == nil {
		t.Error("missing sender accepted")
	}
}
