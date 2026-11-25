package app

import (
	"strings"
	"testing"
)

func TestParseServiceTokens(t *testing.T) {
	long := strings.Repeat("t", 32)
	got, err := parseServiceTokens([]string{"openstack-management-api=" + long, "dynamic-zones=" + long + "2"})
	if err != nil || len(got) != 2 || got[0].Name != "openstack-management-api" {
		t.Fatalf("%+v %v", got, err)
	}
	for name, in := range map[string][]string{
		"no equals":   {"x"},
		"short":       {"a=short"},
		"same token":  {"a=" + long, "b=" + long},
		"same name":   {"a=" + long, "a=" + long + "x"},
		"empty token": {"a="},
	} {
		if _, err := parseServiceTokens(in); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
}

func TestLoadConfigRefusesIncomplete(t *testing.T) {
	t.Setenv("DB_CONNECTION_STRING", "host=x")
	t.Setenv("NOTIFY_SERVICE_TOKENS", "openstack-management-api="+strings.Repeat("t", 32))
	t.Setenv("UNSUBSCRIBE_SECRET", strings.Repeat("u", 32))
	t.Setenv("PUBLIC_URL", "https://notify.example.edu")
	t.Setenv("MAIL_MODE", "log")
	if _, err := loadConfig(); err != nil {
		t.Fatalf("complete config refused: %v", err)
	}
	t.Setenv("MAIL_MODE", "smtp")
	if _, err := loadConfig(); err == nil {
		t.Error("smtp without host accepted")
	}
	t.Setenv("MAIL_MODE", "log")
	t.Setenv("UNSUBSCRIBE_SECRET", "short")
	if _, err := loadConfig(); err == nil {
		t.Error("short unsubscribe secret accepted")
	}
}
