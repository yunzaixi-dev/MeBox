package repository

import (
	"context"
	"strings"
	"time"

	"github.com/truewhile/MeBox/internal/perftrace"
)

const sqliteBusyRetryMaxElapsed = 6 * time.Second

func withSQLiteBusyRetry(ctx context.Context, op func() error) error {
	delay := 25 * time.Millisecond
	deadline := time.Now().Add(sqliteBusyRetryMaxElapsed)
	for {
		attempt := perftrace.Begin(ctx)
		err := op()
		perftrace.End(ctx, "db.sqlite.retry.attempt", attempt)
		if !IsSQLiteBusyError(err) {
			return err
		}
		perftrace.Count(ctx, "db.sqlite.retry.busy")
		if ctxErr := ctx.Err(); ctxErr != nil {
			perftrace.Count(ctx, "db.sqlite.retry.canceled")
			return ctxErr
		}
		if time.Now().Add(delay).After(deadline) {
			perftrace.Count(ctx, "db.sqlite.retry.exhausted")
			return err
		}
		backoff := perftrace.Begin(ctx)
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			perftrace.End(ctx, "db.sqlite.retry.backoff", backoff)
			perftrace.Count(ctx, "db.sqlite.retry.canceled")
			return ctx.Err()
		case <-timer.C:
			perftrace.End(ctx, "db.sqlite.retry.backoff", backoff)
		}
		if delay < 500*time.Millisecond {
			delay *= 2
		}
	}
}

func IsSQLiteBusyError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "sqlite_busy") ||
		strings.Contains(msg, "sqlite_locked") ||
		strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked")
}
