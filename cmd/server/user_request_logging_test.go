package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/truewhile/MeBox/internal/config"
	"go.uber.org/zap"
)

func TestUserRequestLogPrivateAcrossRotation(t *testing.T) {
	t.Setenv("MEBOX_USER_REQUEST_LOG", "true")
	cfg := &config.Config{}
	cfg.App.DataDir = t.TempDir()
	cfg.App.Debug = true
	cfg.Logging.Format = "console"
	log, closeLog, err := newUserRequestLogger(cfg)
	if err != nil {
		t.Fatal(err)
	}
	log.Info("request", zap.String("user_id", "user-one"))
	closeLog()
	dir := filepath.Join(cfg.App.DataDir, "logs", "user-requests")
	path := filepath.Join(dir, "requests.jsonl")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatalf("private sink must stay JSON even with console logs: %v", err)
	}
	if record["user_id"] != "user-one" {
		t.Fatalf("lost user identity: %v", record)
	}
	w := &rotatingFileWriter{path: path, maxSize: 1, maxBackups: 2, mode: 0o600}
	if err := w.open(); err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("next\n")); err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]os.FileMode{dir: 0o700, path: 0o600, path + ".1": 0o600} {
		stat, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if stat.Mode().Perm() != want {
			t.Errorf("%s permissions %o, want %o", p, stat.Mode().Perm(), want)
		}
	}
}

func TestUserRequestLogRefusesPrivatePathSymlinks(t *testing.T) {
	t.Setenv("MEBOX_USER_REQUEST_LOG", "true")
	for _, leaf := range []bool{false, true} {
		t.Run(map[bool]string{false: "directory", true: "file"}[leaf], func(t *testing.T) {
			cfg := &config.Config{}
			cfg.App.DataDir = t.TempDir()
			dir := filepath.Join(cfg.App.DataDir, "logs", "user-requests")
			link := dir
			if leaf {
				link = filepath.Join(dir, "requests.jsonl")
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o700); err != nil {
				t.Fatal(err)
			}
			target := t.TempDir()
			if leaf {
				target = filepath.Join(target, "outside.jsonl")
				if err := os.WriteFile(target, []byte("private outside"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.Symlink(target, link); err != nil {
				t.Fatal(err)
			}
			if _, closeLog, err := newUserRequestLogger(cfg); err == nil {
				closeLog()
				t.Fatal("private sink followed a symlink")
			}
			if leaf {
				data, err := os.ReadFile(target)
				if err != nil || string(data) != "private outside" {
					t.Fatal("changed symlink target")
				}
			}
		})
	}
}
