package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	sqlitedriver "github.com/glebarez/go-sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/truewhile/MeBox/internal/perftrace"
)

// GORM's supplied start includes callbacks, pool acquisition, SQL execution and
// result scanning. Gate and transaction timings overlap it; do not add them.
// The existing logger alone decides whether to render SQL via fc.
type performanceGormLogger struct {
	logger.Interface
}

func (l performanceGormLogger) LogMode(level logger.LogLevel) logger.Interface {
	return performanceGormLogger{Interface: l.Interface.LogMode(level)}
}

func (l performanceGormLogger) Trace(ctx context.Context, begin time.Time, fc func() (string, int64), err error) {
	if perftrace.From(ctx) != nil {
		perftrace.End(ctx, "db.sql", begin)
		recordDatabaseError(ctx, err)
	}
	l.Interface.Trace(ctx, begin, fc, err)
}

func recordDatabaseError(ctx context.Context, err error) {
	if err == nil {
		return
	}
	perftrace.Count(ctx, "db.error")
	switch {
	case errors.Is(err, context.Canceled):
		perftrace.Count(ctx, "db.error.canceled")
	case errors.Is(err, context.DeadlineExceeded):
		perftrace.Count(ctx, "db.error.deadline")
	case errors.Is(err, gorm.ErrRecordNotFound):
		perftrace.Count(ctx, "db.error.not_found")
	default:
		var sqliteErr *sqlitedriver.Error
		if errors.As(err, &sqliteErr) {
			// Extended SQLite result codes retain the primary code in low bits.
			switch sqliteErr.Code() & 0xff {
			case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
				perftrace.Count(ctx, "db.error.sqlite_busy")
				return
			case 19: // SQLITE_CONSTRAINT
				perftrace.Count(ctx, "db.error.constraint")
				return
			}
		}
		perftrace.Count(ctx, "db.error.other")
	}
}

func installDatabasePerformance(db *gorm.DB) {
	if !isSQLite(db) {
		return
	}
	// Open always enables prepared statements. Keep that object and its cache
	// intact, decorating only its native pool's BeginTx entry point.
	if prepared, ok := db.ConnPool.(*gorm.PreparedStmtDB); ok {
		if pool, ok := prepared.ConnPool.(*sql.DB); ok {
			prepared.ConnPool = &performanceSQLDB{DB: pool}
		}
	}
}

type performanceSQLDB struct {
	*sql.DB
}

func (db *performanceSQLDB) GetDBConn() (*sql.DB, error) {
	return db.DB, nil
}

// BeginTx includes connection-pool acquisition and SQLite's immediate write
// lock wait. Pool Stats must be reported as global counters, not per-request.
// Returning the native *sql.Tx preserves GORM's commit/savepoint semantics.
func (db *performanceSQLDB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error) {
	start := perftrace.Begin(ctx)
	tx, err := db.DB.BeginTx(ctx, opts)
	perftrace.End(ctx, "db.sqlite.transaction.begin", start)
	if !start.IsZero() {
		recordDatabaseError(ctx, err)
	}
	return tx, err
}
