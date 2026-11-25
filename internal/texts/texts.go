// Package texts turns events into what people read: a title and a paragraph per event type in German and English, and the mails built from them (an immediate mail of one type, possibly bundling several events, and the summary).
//
// The German texts address nobody directly where they can avoid it, like the portal; where they cannot, they say "Sie".
package texts

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/pfisterer/notifications/internal/model"
)

// Lang is de or en.
type Lang string

const (
	DE Lang = "de"
	EN Lang = "en"
)

// ParseLang maps anything but "en" to German.
func ParseLang(s string) Lang {
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(s)), "en") {
		return EN
	}
	return DE
}

// View is the part of an event the texts read.
type View struct {
	Type   string
	Object model.Object
	Data   map[string]any
	Due    *time.Time
	// Reminder is 0 for the event itself, 1 or 2 for a reminder of it.
	Reminder int
	// Loc is the time zone dates are written in.
	Loc *time.Location
}

// S is the data field key as a string ("" when missing or not a scalar).
func (v View) S(key string) string {
	switch x := v.Data[key].(type) {
	case string:
		return strings.TrimSpace(x)
	case float64:
		return strings.TrimSuffix(strings.TrimRight(fmt.Sprintf("%f", x), "0"), ".")
	case bool:
		return fmt.Sprint(x)
	}
	return ""
}

// L is the data field key in language l: a plain string, or an object {"de": …, "en": …}.
func (v View) L(key string, l Lang) string {
	if m, ok := v.Data[key].(map[string]any); ok {
		if s, ok := m[string(l)].(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
		for _, other := range []Lang{DE, EN} {
			if s, ok := m[string(other)].(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
		return ""
	}
	return v.S(key)
}

// name is the object's name, its id, or a neutral word.
func (v View) name(l Lang) string {
	switch {
	case v.Object.Name != "":
		return v.Object.Name
	case v.Object.ID != "":
		return v.Object.ID
	}
	return tr(l, "ohne Namen", "unnamed")
}

// date writes a date field (RFC 3339 or YYYY-MM-DD in data) or else the due date, as the language writes dates; "" when there is none.
func (v View) date(key string, l Lang) string {
	var t time.Time
	if s := v.S(key); s != "" {
		if p, err := time.Parse(time.RFC3339, s); err == nil {
			t = p
		} else if p, err := time.Parse("2006-01-02", s); err == nil {
			t = p
		}
	}
	if t.IsZero() && v.Due != nil {
		t = *v.Due
	}
	if t.IsZero() {
		return ""
	}
	return FormatDate(t, v.Loc, l)
}

// FormatDate writes t's day in loc the way l writes dates.
func FormatDate(t time.Time, loc *time.Location, l Lang) string {
	if loc != nil {
		t = t.In(loc)
	}
	if l == EN {
		return t.Format("2 January 2006")
	}
	return fmt.Sprintf("%d. %s %d", t.Day(), germanMonths[t.Month()-1], t.Year())
}

var germanMonths = []string{"Januar", "Februar", "März", "April", "Mai", "Juni", "Juli", "August", "September", "Oktober", "November", "Dezember"}

func tr(l Lang, de, en string) string {
	if l == EN {
		return en
	}
	return de
}

// Kind is the text of one event type.
type Kind struct {
	Type string
	// Class is the class senders use for it; only shown in the settings.
	Class model.Class
	// Name is how the settings list the type.
	Name func(l Lang) string
	// Title is one line: the subject of a single mail and the heading of an item.
	Title func(v View, l Lang) string
	// Body is the paragraph under the title.
	Body func(v View, l Lang) string
	// Bundle is the subject of a mail with n > 1 events of this type.
	Bundle func(n int, l Lang) string
	// Link is the label of the button to the object.
	Link func(l Lang) string
}

func names(de, en string) func(Lang) string { return func(l Lang) string { return tr(l, de, en) } }

// kinds are the event types with their own text. Anything else is delivered with generic().
var kinds = map[string]Kind{
	"approval.waiting": {
		Type: "approval.waiting", Class: model.ClassAction,
		Name: names("Antrag wartet auf Entscheidung", "Request awaiting a decision"),
		Title: func(v View, l Lang) string {
			return tr(l, "Antrag wartet auf Entscheidung: ", "Request awaiting a decision: ") + v.name(l)
		},
		Body: func(v View, l Lang) string {
			who := v.S("requester")
			if who == "" {
				who = tr(l, "Jemand", "Somebody")
			}
			var b strings.Builder
			if what := v.L("summary", l); what != "" {
				b.WriteString(tr(l, fmt.Sprintf("%s hat für „%s“ %s beantragt", who, v.name(l), what), fmt.Sprintf("%s has requested %s for “%s”", who, what, v.name(l))))
			} else {
				b.WriteString(tr(l, fmt.Sprintf("%s hat für „%s“ einen Antrag gestellt", who, v.name(l)), fmt.Sprintf("%s has filed a request for “%s”", who, v.name(l))))
			}
			if budget := v.S("budget"); budget != "" {
				b.WriteString(tr(l, fmt.Sprintf(" (Budget „%s“)", budget), fmt.Sprintf(" (budget “%s”)", budget)))
			}
			b.WriteString(tr(l, ". Er wartet auf eine Entscheidung: genehmigen oder ablehnen.", ". It is waiting for a decision: approve or reject."))
			if reason := v.L("reason", l); reason != "" {
				b.WriteString(tr(l, " Begründung: ", " Reason given: ") + reason)
			}
			return b.String()
		},
		Bundle: func(n int, l Lang) string {
			return tr(l, fmt.Sprintf("%d Anträge warten auf Entscheidung", n), fmt.Sprintf("%d requests await a decision", n))
		},
		Link: names("Antrag ansehen", "View request"),
	},
	"approval.decided": {
		Type: "approval.decided", Class: model.ClassInfo,
		Name: names("Antrag entschieden", "Request decided"),
		Title: func(v View, l Lang) string {
			switch v.S("decision") {
			case "rejected":
				return tr(l, "Antrag abgelehnt: ", "Request rejected: ") + v.name(l)
			case "partially_approved":
				return tr(l, "Antrag teilweise genehmigt: ", "Request partly approved: ") + v.name(l)
			}
			return tr(l, "Antrag genehmigt: ", "Request approved: ") + v.name(l)
		},
		Body: func(v View, l Lang) string {
			var verbDE, verbEN string
			switch v.S("decision") {
			case "rejected":
				verbDE, verbEN = "abgelehnt", "rejected"
			case "partially_approved":
				verbDE, verbEN = "teilweise genehmigt", "partly approved"
			default:
				verbDE, verbEN = "genehmigt", "approved"
			}
			s := tr(l, fmt.Sprintf("Der Antrag für „%s“ wurde %s", v.name(l), verbDE), fmt.Sprintf("The request for “%s” was %s", v.name(l), verbEN))
			if by := v.S("decided_by"); by != "" {
				s += tr(l, " von "+by, " by "+by)
			}
			s += "."
			if what := v.L("summary", l); what != "" {
				s += " " + what
			}
			if c := v.L("comment", l); c != "" {
				s += tr(l, " Kommentar: ", " Comment: ") + c
			}
			return s
		},
		Bundle: func(n int, l Lang) string {
			return tr(l, fmt.Sprintf("%d Anträge entschieden", n), fmt.Sprintf("%d requests decided", n))
		},
		Link: names("Projekt ansehen", "View project"),
	},
	"project.ending": {
		Type: "project.ending", Class: model.ClassAction,
		Name: names("Projekt endet bald", "Project ending soon"),
		Title: func(v View, l Lang) string {
			if d := v.date("end", l); d != "" {
				return tr(l, fmt.Sprintf("Projekt endet am %s: %s", d, v.name(l)), fmt.Sprintf("Project ends on %s: %s", d, v.name(l)))
			}
			return tr(l, "Projekt endet bald: ", "Project ends soon: ") + v.name(l)
		},
		Body: func(v View, l Lang) string {
			d := v.date("end", l)
			if d == "" {
				d = tr(l, "in Kürze", "soon")
			} else {
				d = tr(l, "am "+d, "on "+d)
			}
			return tr(l,
				fmt.Sprintf("Das Projekt „%s“ endet %s. Danach wird es gelöscht und seine Ressourcen werden freigegeben. Wird es länger gebraucht, kann im Portal eine Verlängerung beantragt werden.", v.name(l), d),
				fmt.Sprintf("The project “%s” ends %s. After that it is deleted and its resources are released. If it is needed for longer, an extension can be requested in the portal.", v.name(l), d))
		},
		Bundle: func(n int, l Lang) string {
			return tr(l, fmt.Sprintf("%d Projekte enden bald", n), fmt.Sprintf("%d projects end soon", n))
		},
		Link: names("Projekt ansehen", "View project"),
	},
	"allocation.expired": {
		Type: "allocation.expired", Class: model.ClassAction,
		Name: names("Zuteilung abgelaufen", "Allocation expired"),
		Title: func(v View, l Lang) string {
			return tr(l, "Zuteilung abgelaufen: ", "Allocation expired: ") + v.name(l)
		},
		Body: func(v View, l Lang) string {
			budget := v.S("budget")
			var s string
			if budget != "" {
				s = tr(l, fmt.Sprintf("Die Zuteilung aus dem Budget „%s“ für das Projekt „%s“ ist abgelaufen.", budget, v.name(l)), fmt.Sprintf("The allocation from the budget “%s” for the project “%s” has expired.", budget, v.name(l)))
			} else {
				s = tr(l, fmt.Sprintf("Eine Zuteilung des Projekts „%s“ ist abgelaufen.", v.name(l)), fmt.Sprintf("An allocation of the project “%s” has expired.", v.name(l)))
			}
			if what := v.L("summary", l); what != "" {
				s += " " + what
			}
			if d := v.date("shrink_at", l); d != "" {
				s += tr(l,
					fmt.Sprintf(" Das Projekt behält seine Ressourcen vorerst. Am %s wird es um diese Zuteilung verkleinert, wenn sie bis dahin nicht verlängert oder einem anderen Budget zugeordnet wird.", d),
					fmt.Sprintf(" The project keeps its resources for now. On %s it will be reduced by this allocation unless the allocation is extended or moved to another budget by then.", d))
			} else {
				s += tr(l, " Das Projekt behält seine Ressourcen vorerst; die Zuteilung kann verlängert oder einem anderen Budget zugeordnet werden.", " The project keeps its resources for now; the allocation can be extended or moved to another budget.")
			}
			return s
		},
		Bundle: func(n int, l Lang) string {
			return tr(l, fmt.Sprintf("%d Zuteilungen abgelaufen", n), fmt.Sprintf("%d allocations expired", n))
		},
		Link: names("Projekt ansehen", "View project"),
	},
	"allocation.shortened": {
		Type: "allocation.shortened", Class: model.ClassDisruption,
		Name: names("Zuteilung verringert", "Allocation reduced"),
		Title: func(v View, l Lang) string {
			return tr(l, "Zuteilung verringert: ", "Allocation reduced: ") + v.name(l)
		},
		Body: func(v View, l Lang) string {
			budget := v.S("budget")
			var s string
			if budget != "" {
				s = tr(l, fmt.Sprintf("Die Zuteilung aus dem Budget „%s“ für das Projekt „%s“ wurde verringert", budget, v.name(l)), fmt.Sprintf("The allocation from the budget “%s” for the project “%s” was reduced", budget, v.name(l)))
			} else {
				s = tr(l, fmt.Sprintf("Eine Zuteilung des Projekts „%s“ wurde verringert", v.name(l)), fmt.Sprintf("An allocation of the project “%s” was reduced", v.name(l)))
			}
			if by := v.S("changed_by"); by != "" {
				s += tr(l, " von "+by, " by "+by)
			}
			s += "."
			if what := v.L("summary", l); what != "" {
				s += " " + what
			}
			if r := v.L("reason", l); r != "" {
				s += tr(l, " Begründung: ", " Reason: ") + r
			}
			return s
		},
		Bundle: func(n int, l Lang) string {
			return tr(l, fmt.Sprintf("%d Zuteilungen verringert", n), fmt.Sprintf("%d allocations reduced", n))
		},
		Link: names("Projekt ansehen", "View project"),
	},
}

// generic is the text of a type without its own: data.title and data.text when the sender gives them (a string or {"de","en"}).
func generic(typ string) Kind {
	return Kind{
		Type: typ, Class: model.ClassInfo,
		Name: func(Lang) string { return typ },
		Title: func(v View, l Lang) string {
			if t := v.L("title", l); t != "" {
				return t
			}
			if v.Object.Name != "" || v.Object.ID != "" {
				return tr(l, "Benachrichtigung zu ", "Notification about ") + v.name(l)
			}
			return tr(l, "Benachrichtigung: ", "Notification: ") + typ
		},
		Body: func(v View, l Lang) string { return "" },
		Bundle: func(n int, l Lang) string {
			return tr(l, fmt.Sprintf("%d Benachrichtigungen (%s)", n, typ), fmt.Sprintf("%d notifications (%s)", n, typ))
		},
		Link: names("Ansehen", "View"),
	}
}

// KindOf is the text of type typ, generic() when it has none.
func KindOf(typ string) Kind {
	if k, ok := kinds[typ]; ok {
		return k
	}
	return generic(typ)
}

// Known lists the types with their own text, sorted, for the settings.
func Known() []Kind {
	out := make([]Kind, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Type < out[j].Type })
	return out
}

// Item is one event rendered in one language.
type Item struct {
	Title string
	Body  string
	URL   string
	Link  string
}

// Render renders v in l. data.text, when given, is added to the paragraph of every type.
func Render(v View, l Lang) Item {
	k := KindOf(v.Type)
	it := Item{Title: k.Title(v, l), Body: k.Body(v, l), URL: v.Object.URL, Link: k.Link(l)}
	if extra := v.L("text", l); extra != "" {
		if it.Body != "" {
			it.Body += "\n\n"
		}
		it.Body += extra
	}
	if v.Reminder > 0 {
		it.Title = tr(l, "Erinnerung: ", "Reminder: ") + it.Title
	}
	return it
}
