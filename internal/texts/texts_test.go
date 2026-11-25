package texts

import (
	"strings"
	"testing"
	"time"

	"github.com/pfisterer/notifications/internal/model"
)

// Every type with its own text renders a title and a paragraph in both languages, and the two differ.
func TestEveryKnownTypeInBothLanguages(t *testing.T) {
	due := time.Date(2026, 12, 9, 0, 0, 0, 0, time.UTC)
	for _, k := range Known() {
		v := View{Type: k.Type, Object: model.Object{Name: "Kursprojekt", URL: "https://portal.example.edu/p/1"}, Due: &due, Data: map[string]any{"budget": "DSKI", "requester": "a@example.edu", "summary": "4 Cores"}}
		de, en := Render(v, DE), Render(v, EN)
		for _, it := range []Item{de, en} {
			if it.Title == "" || it.Body == "" || it.Link == "" || !strings.Contains(it.Title, "Kursprojekt") {
				t.Errorf("%s: incomplete %+v", k.Type, it)
			}
		}
		if de.Title == en.Title || de.Body == en.Body || k.Name(DE) == "" || k.Name(EN) == "" {
			t.Errorf("%s: not translated", k.Type)
		}
		if b := k.Bundle(3, DE); !strings.HasPrefix(b, "3 ") {
			t.Errorf("%s: bundle %q", k.Type, b)
		}
	}
	for _, typ := range []string{"approval.waiting", "approval.decided", "project.ending", "allocation.expired", "allocation.shortened"} {
		if KindOf(typ).Type != typ || KindOf(typ).Name(DE) == typ {
			t.Errorf("%s has no text of its own", typ)
		}
	}
}

func TestGenericAndReminder(t *testing.T) {
	v := View{Type: "batch.finished", Data: map[string]any{"title": map[string]any{"de": "Job fertig", "en": "Job done"}, "text": "Ergebnis liegt bereit."}, Reminder: 1}
	it := Render(v, EN)
	if it.Title != "Reminder: Job done" || it.Body != "Ergebnis liegt bereit." {
		t.Errorf("%+v", it)
	}
	if it := Render(View{Type: "x.y"}, DE); it.Title != "Benachrichtigung: x.y" {
		t.Errorf("fallback %q", it.Title)
	}
}

func TestMailEscapesAndCarriesLinks(t *testing.T) {
	items := []Item{{Title: "<script>x</script>", Body: "a & b", URL: "https://portal.example.edu/p?a=1&b=2", Link: "Ansehen"}}
	c := Immediate(DE, "x.y", items, false, Footer{SettingsURL: "https://portal.example.edu/s", UnsubscribeURL: "https://n.example.edu/unsubscribe?token=t", UnsubscribeWhat: "diese Art"})
	if strings.Contains(c.HTML, "<script>") || !strings.Contains(c.HTML, "&lt;script&gt;") {
		t.Error("HTML not escaped")
	}
	if !strings.Contains(c.Text, "https://n.example.edu/unsubscribe?token=t") || !strings.Contains(c.HTML, "https://portal.example.edu/s") {
		t.Error("footer links missing")
	}
}
