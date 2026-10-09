package database

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/truewhile/MeBox/internal/config"
	"github.com/truewhile/MeBox/internal/perftrace"
)

func openPerformanceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	cfg := &config.Config{}
	cfg.Database.Type = "sqlite"
	cfg.Database.DBPath = ":memory:"
	cfg.Database.MaxOpenConns = 1
	cfg.Database.MaxIdleConns = 1
	db, err := Open(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Exec("CREATE TABLE trace_items (id INTEGER PRIMARY KEY, value TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func databaseMetric(t *testing.T, snapshot perftrace.Snapshot, name string) perftrace.Metric {
	t.Helper()
	for _, metric := range snapshot.Metrics {
		if metric.Name == name {
			return metric
		}
	}
	t.Fatalf("missing %s metric in %#v", name, snapshot.Metrics)
	return perftrace.Metric{}
}

func TestDatabasePerformanceSQL(t *testing.T) {
	db := openPerformanceTestDB(t)
	ctx, trace := perftrace.New(context.Background())
	// Silent suppresses SQL rendering, not request performance metrics.
	db = db.WithContext(ctx).Session(&gorm.Session{Logger: db.Logger.LogMode(logger.Silent)})
	item := struct {
		ID    int
		Value string
	}{ID: 1, Value: "private parameter"}
	if err := db.Table("trace_items").Create(&item).Error; err != nil {
		t.Fatal(err)
	}
	var got struct {
		ID    int
		Value string
	}
	if err := db.Table("trace_items").First(&got, 1).Error; err != nil || got.Value != item.Value {
		t.Fatalf("query: value=%q error=%v", got.Value, err)
	}
	if err := db.Table("trace_items").Where("id = ?", 1).Update("value", "updated").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Table("trace_items").Where("id = ?", 1).Delete(&item).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO trace_items VALUES (?, ?)", 2, "private parameter").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO trace_items VALUES (?, ?)", 2, "duplicate").Error; err == nil {
		t.Fatal("duplicate insert should retain its SQLite constraint error")
	}
	got.ID = 0
	if err := db.Table("trace_items").First(&got, 99).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("missing row error = %v", err)
	}
	var count int
	if err := db.Raw("SELECT COUNT(*) FROM trace_items").Scan(&count).Error; err != nil || count != 1 {
		t.Fatalf("raw count=%d error=%v", count, err)
	}
	snapshot := trace.Finish()
	if metric := databaseMetric(t, snapshot, "db.sql"); metric.Count != 8 || metric.TotalNS <= 0 {
		t.Fatalf("SQL metric = %#v, want eight timed statements", metric)
	}
	for name, want := range map[string]int64{
		"db.sqlite.write_gate.wait":   5,
		"db.sqlite.transaction.begin": 3,
		"db.error":                    2,
		"db.error.constraint":         1,
		"db.error.not_found":          1,
	} {
		if metric := databaseMetric(t, snapshot, name); metric.Count != want {
			t.Fatalf("%s count=%d, want %d", name, metric.Count, want)
		}
	}
}

func TestDatabasePerformanceTransactions(t *testing.T) {
	db := openPerformanceTestDB(t)
	ctx, trace := perftrace.New(context.Background())
	rollback := errors.New("rollback this transaction")
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("INSERT INTO trace_items VALUES (1, 'outer')").Error; err != nil {
			return err
		}
		if err := tx.Transaction(func(nested *gorm.DB) error {
			if err := nested.Exec("INSERT INTO trace_items VALUES (2, 'nested')").Error; err != nil {
				return err
			}
			return rollback
		}); !errors.Is(err, rollback) {
			return errors.New("nested transaction lost rollback error")
		}
		return tx.Exec("INSERT INTO trace_items VALUES (3, 'outer')").Error
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec("INSERT INTO trace_items VALUES (4, 'rollback')").Error; err != nil {
			return err
		}
		return rollback
	}); !errors.Is(err, rollback) {
		t.Fatalf("outer rollback error = %v", err)
	}
	// An untraced native transaction still commits normally.
	if err := db.Transaction(func(tx *gorm.DB) error {
		return tx.Exec("INSERT INTO trace_items VALUES (5, 'untraced')").Error
	}); err != nil {
		t.Fatal(err)
	}
	var ids []int
	if err := db.Table("trace_items").Order("id").Pluck("id", &ids).Error; err != nil {
		t.Fatal(err)
	}
	if len(ids) != 3 || ids[0] != 1 || ids[1] != 3 || ids[2] != 5 {
		t.Fatalf("committed IDs=%v, want [1 3 5]", ids)
	}
	if metric := databaseMetric(t, trace.Finish(), "db.sqlite.transaction.begin"); metric.Count != 2 || metric.TotalNS <= 0 {
		t.Fatalf("transaction begin metric = %#v", metric)
	}
}

func TestDatabasePerformanceWriteGateCancellation(t *testing.T) {
	db := openPerformanceTestDB(t).Session(&gorm.Session{SkipDefaultTransaction: true})
	type holderKey struct{}
	entered := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	if err := db.Callback().Create().After("mebox:sqlite_write_lock").Before("gorm:create").Register("test:hold_write_gate", func(tx *gorm.DB) {
		if tx.Statement.Context.Value(holderKey{}) != nil {
			close(entered)
			<-release
		}
	}); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		item := struct{ ID int }{ID: 1}
		done <- db.WithContext(context.WithValue(context.Background(), holderKey{}, true)).Table("trace_items").Create(&item).Error
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("first SQLite statement did not acquire its write gate")
	}
	deadlineCtx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	ctx, trace := perftrace.New(deadlineCtx)
	item := struct{ ID int }{ID: 2}
	if err := db.WithContext(ctx).Table("trace_items").Create(&item).Error; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked write error=%v, want deadline exceeded", err)
	}
	snapshot := trace.Finish()
	if metric := databaseMetric(t, snapshot, "db.sqlite.write_gate.wait"); metric.Count != 1 || metric.TotalNS < int64(20*time.Millisecond) {
		t.Fatalf("write gate wait=%#v, want canceled queued statement", metric)
	}
	if metric := databaseMetric(t, snapshot, "db.sqlite.write_gate.error"); metric.Count != 1 {
		t.Fatalf("write gate errors=%d, want 1", metric.Count)
	}
	// Let the holder reach SQLite and confirm the canceled write did not insert.
	release <- struct{}{}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Table("trace_items").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("remaining rows=%d error=%v", count, err)
	}
}
