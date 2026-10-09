package repository

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/truewhile/MeBox/internal/config"
	"github.com/truewhile/MeBox/internal/database"
	"github.com/truewhile/MeBox/internal/perftrace"
)

func TestSQLiteBusyRetryPerformanceRealTransaction(t *testing.T) {
	cfg := &config.Config{}
	cfg.Database.Type = "sqlite"
	cfg.Database.DBPath = filepath.Join(t.TempDir(), "retry.db")
	cfg.Database.WALMode = true
	cfg.Database.BusyTimeout = 1
	cfg.Database.MaxOpenConns = 2
	cfg.Database.MaxIdleConns = 2
	db, err := database.Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	db = db.Session(&gorm.Session{Logger: db.Logger.LogMode(logger.Silent)})
	if err := db.Exec("CREATE TABLE retry_items (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	// A native immediate transaction holds the real SQLite writer lock.
	holder := db.Begin()
	if holder.Error != nil {
		t.Fatal(holder.Error)
	}
	defer holder.Rollback()
	deadlineCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	ctx, trace := perftrace.New(deadlineCtx)
	attempts := 0
	err = withSQLiteBusyRetry(ctx, func() error {
		attempts++
		err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return tx.Exec("INSERT INTO retry_items VALUES (1)").Error
		})
		if attempts == 1 && IsSQLiteBusyError(err) {
			if releaseErr := holder.Rollback().Error; releaseErr != nil {
				t.Fatalf("release SQLite writer lock: %v", releaseErr)
			}
		}
		return err
	})
	if err != nil || attempts != 2 {
		t.Fatalf("retry attempts=%d error=%v, want busy then commit", attempts, err)
	}
	var count int64
	if err := db.Table("retry_items").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("committed rows=%d error=%v", count, err)
	}
	metrics := make(map[string]perftrace.Metric)
	for _, metric := range trace.Finish().Metrics {
		metrics[metric.Name] = metric
	}
	for name, want := range map[string]int64{
		"db.sqlite.retry.attempt":     2,
		"db.sqlite.retry.busy":        1,
		"db.sqlite.retry.backoff":     1,
		"db.sqlite.transaction.begin": 2,
		"db.error.sqlite_busy":        1,
	} {
		if metric := metrics[name]; metric.Count != want {
			t.Fatalf("%s count=%d, want %d", name, metric.Count, want)
		}
	}
	if metric := metrics["db.sqlite.retry.backoff"]; metric.TotalNS < int64(20*time.Millisecond) {
		t.Fatalf("backoff=%#v, want real retry delay", metric)
	}
}
