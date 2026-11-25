package unsub

import (
	"strings"
	"testing"
)

func TestSignVerify(t *testing.T) {
	s, err := New(strings.Repeat("a", 32))
	if err != nil {
		t.Fatal(err)
	}
	tok := s.Sign(Claim{Recipient: "a@example.edu", Type: "project.ending"})
	c, err := s.Verify(tok)
	if err != nil || c.Recipient != "a@example.edu" || c.Type != "project.ending" {
		t.Fatalf("%+v %v", c, err)
	}
	other, _ := New(strings.Repeat("b", 32))
	if _, err := other.Verify(tok); err == nil {
		t.Error("token of another secret accepted")
	}
	p, sig, _ := strings.Cut(tok, ".")
	if _, err := s.Verify(p[:len(p)-2] + "xx." + sig); err == nil {
		t.Error("changed payload accepted")
	}
	if _, err := New("short"); err == nil {
		t.Error("short secret accepted")
	}
}
