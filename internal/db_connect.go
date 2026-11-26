package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pfisterer/cloud-self-service-golib/retry"
	"go.uber.org/zap"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// dbStartupPolicy is how long a starting pod waits for its database: up to a minute, pauses from 1 s doubling up to 5 s. kube-router lets a new pod's address through the NetworkPolicy to Postgres only a moment after the pod starts and rejects it until then (icmp port unreachable, seen by the client as "connection refused"), so the first attempt usually fails although nothing is wrong; the same holds when the database comes up after this pod.
var dbStartupPolicy = retry.Policy{Total: time.Minute, Initial: time.Second, Max: 5 * time.Second, Jitter: 0.1}

// openDB opens and pings the database, tried again under dbStartupPolicy while an attempt fails for a reason that waiting can fix. A refused attempt is logged at warn level; GORM's own logger is discarded during the attempts, because GORM logs a failed gorm.Open as an error ("failed to initialize database"), which would raise the AppLogError alert at every pod start. Once connected, GORM logs warnings again.
func openDB(ctx context.Context, dsn string, log *zap.SugaredLogger) (*gorm.DB, error) {
	p := dbStartupPolicy
	p.OnRetry = func(attempt int, err error, wait time.Duration) {
		log.Warnw("database not reachable yet, trying again", "attempt", attempt, "retry_in", wait.Round(time.Millisecond).String(), "error", err)
	}
	var db *gorm.DB
	attempts := 0
	err := retry.Do(ctx, p, func(context.Context) error {
		attempts++
		d, err := gorm.Open(postgres.Open(dsn), &gorm.Config{TranslateError: true, Logger: gormlogger.Discard})
		if err == nil {
			var sqlDB interface{ Ping() error }
			if sqlDB, err = d.DB(); err == nil {
				err = sqlDB.Ping()
			}
		}
		if err != nil {
			if !retryableConnectError(err) {
				return retry.Permanent(err)
			}
			return err
		}
		db = d
		return nil
	})
	if err != nil {
		if attempts > 1 {
			return nil, fmt.Errorf("gave up after %d attempts: %w", attempts, err)
		}
		return nil, err
	}
	db.Logger = gormlogger.Default.LogMode(gormlogger.Warn)
	if sqlDB, err := db.DB(); err == nil {
		sqlDB.SetMaxOpenConns(10)
	}
	return db, nil
}

// retryableConnectError tells whether a failed attempt may succeed later: the database was not reachable (refused, reset, timed out, name not resolved) or said it cannot take the connection yet (starting up, too many connections). A server answer that rejects the login (wrong password, unknown role or database) and a DSN that does not parse are final.
func retryableConnectError(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 57P03 cannot_connect_now (starting up or shutting down), 53300 too_many_connections.
		return pgErr.Code == "57P03" || pgErr.Code == "53300"
	}
	var parseErr *pgconn.ParseConfigError
	return !errors.As(err, &parseErr)
}
