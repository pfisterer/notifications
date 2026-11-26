package app

import (
	"fmt"
	"syscall"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// A refused or unreachable database is worth waiting for; a rejected login or a broken DSN is not.
func TestRetryableConnectError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"refused", fmt.Errorf("dial tcp 10.43.0.1:5432: %w", syscall.ECONNREFUSED), true},
		{"starting up", &pgconn.PgError{Code: "57P03"}, true},
		{"too many connections", &pgconn.PgError{Code: "53300"}, true},
		{"wrong password", &pgconn.PgError{Code: "28P01"}, false},
		{"missing database", fmt.Errorf("connect: %w", &pgconn.PgError{Code: "3D000"}), false},
	}
	_, parseErr := pgconn.ParseConfig("postgres://%zz")
	if parseErr == nil {
		t.Fatal("expected a parse error for a broken DSN")
	}
	cases = append(cases, struct {
		name string
		err  error
		want bool
	}{"bad dsn", parseErr, false})
	for _, c := range cases {
		if got := retryableConnectError(c.err); got != c.want {
			t.Errorf("%s: retryableConnectError = %v, want %v", c.name, got, c.want)
		}
	}
}
