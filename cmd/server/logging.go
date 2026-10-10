package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/truewhile/MeBox/internal/config"
)

// newLogger 根据 cfg.Logging 构建 Zap。
func newLogger(cfg *config.Config) (*zap.Logger, error) {
	log, _, err := newLoggerWithCloser(cfg)
	return log, err
}

func newLoggerWithCloser(cfg *config.Config) (*zap.Logger, func(), error) {
	if cfg.App.Debug {
		log, err := zap.NewDevelopment()
		return log, func() {}, err
	}
	level := configuredLogLevel(cfg.Logging.Level)
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	var encoder zapcore.Encoder
	if strings.EqualFold(strings.TrimSpace(cfg.Logging.Format), "console") {
		encoder = zapcore.NewConsoleEncoder(encoderCfg)
	} else {
		encoder = zapcore.NewJSONEncoder(encoderCfg)
	}
	cores := []zapcore.Core{
		zapcore.NewCore(encoder, zapcore.Lock(os.Stdout), level),
	}
	var closers []func() error
	appPath, warnPath, errorPath := logFilePaths(cfg)
	if appPath != "" {
		appWriter, err := newRotatingFileWriter(appPath, cfg.Logging)
		if err != nil {
			return nil, nil, err
		}
		cores = append(cores, zapcore.NewCore(encoder, appWriter, level))
		closers = append(closers, appWriter.Close)
	}
	if warnPath != "" {
		warnWriter, err := newRotatingFileWriter(warnPath, cfg.Logging)
		if err != nil {
			return nil, nil, err
		}
		cores = append(cores, zapcore.NewCore(encoder, warnWriter, zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
			return lvl == zapcore.WarnLevel && level.Enabled(lvl)
		})))
		closers = append(closers, warnWriter.Close)
	}
	if errorPath != "" {
		errorWriter, err := newRotatingFileWriter(errorPath, cfg.Logging)
		if err != nil {
			return nil, nil, err
		}
		cores = append(cores, zapcore.NewCore(encoder, errorWriter, zap.LevelEnablerFunc(func(lvl zapcore.Level) bool {
			return lvl >= zapcore.ErrorLevel && level.Enabled(lvl)
		})))
		closers = append(closers, errorWriter.Close)
	}
	closeFn := func() {
		for _, c := range closers {
			_ = c()
		}
	}
	return zap.New(zapcore.NewTee(cores...), zap.AddCaller(), zap.AddStacktrace(zapcore.ErrorLevel), zap.ErrorOutput(zapcore.Lock(os.Stderr))), closeFn, nil
}

func configuredLogLevel(raw string) zapcore.Level {
	level := zapcore.WarnLevel
	raw = strings.TrimSpace(raw)
	if raw != "" {
		var parsed zapcore.Level
		if err := parsed.UnmarshalText([]byte(raw)); err == nil {
			level = parsed
		}
	}
	return level
}

func logFilePaths(cfg *config.Config) (string, string, string) {
	out := strings.TrimSpace(cfg.Logging.OutputPath)
	if strings.EqualFold(out, "stdout") || strings.EqualFold(out, "stderr") {
		return "", "", ""
	}
	if out == "" {
		out = filepath.Join(cfg.App.DataDir, "logs")
	}
	if ext := filepath.Ext(out); ext != "" {
		base := strings.TrimSuffix(out, ext)
		return out, base + ".warn" + ext, base + ".error" + ext
	}
	return filepath.Join(out, "app.log"), filepath.Join(out, "warn.log"), filepath.Join(out, "error.log")
}

// newEmbyCompatLogger 构建只写入 Emby 兼容日志文件的独立 Zap 实例。
// 它不参与 app.log 的日志级别过滤，始终记录 INFO 及以上，确保成功请求也能
// 用于还原客户端的接口调用顺序；轮转参数沿用 logging 配置。
func newEmbyCompatLogger(cfg *config.Config) (*zap.Logger, func(), error) {
	encoderCfg := zap.NewProductionEncoderConfig()
	encoderCfg.EncodeTime = zapcore.ISO8601TimeEncoder
	var encoder zapcore.Encoder
	if strings.EqualFold(strings.TrimSpace(cfg.Logging.Format), "console") {
		encoder = zapcore.NewConsoleEncoder(encoderCfg)
	} else {
		encoder = zapcore.NewJSONEncoder(encoderCfg)
	}

	writer, err := newRotatingFileWriter(embyCompatLogPath(cfg), cfg.Logging)
	if err != nil {
		return nil, nil, err
	}
	log := zap.New(
		zapcore.NewCore(encoder, writer, zap.InfoLevel),
		zap.AddCaller(),
		zap.AddStacktrace(zapcore.ErrorLevel),
		zap.ErrorOutput(zapcore.Lock(os.Stderr)),
	)
	return log, func() { _ = writer.Close() }, nil
}

func embyCompatLogPath(cfg *config.Config) string {
	out := strings.TrimSpace(cfg.Logging.OutputPath)
	if out == "" || strings.EqualFold(out, "stdout") || strings.EqualFold(out, "stderr") {
		return filepath.Join(cfg.App.DataDir, "logs", "emby-compat.log")
	}
	if ext := filepath.Ext(out); ext != "" {
		base := strings.TrimSuffix(out, ext)
		return base + ".emby-compat" + ext
	}
	return filepath.Join(out, "emby-compat.log")
}

// newUserRequestLogger is independent of ordinary log levels and never writes plaintext requests to stdout.
func newUserRequestLogger(cfg *config.Config) (*zap.Logger, func(), error) {
	raw := os.Getenv("MEBOX_USER_REQUEST_LOG")
	if raw == "" {
		return nil, nil, nil
	}
	enabled, err := strconv.ParseBool(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid MEBOX_USER_REQUEST_LOG: %w", err)
	}
	if !enabled {
		return nil, nil, nil
	}
	dir := filepath.Join(cfg.App.DataDir, "logs", "user-requests")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, err
	}
	stat, err := os.Lstat(dir)
	if err != nil {
		return nil, nil, err
	}
	if !stat.IsDir() || stat.Mode()&os.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("private request log directory is not a directory")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, nil, err
	}
	writer := &rotatingFileWriter{path: filepath.Join(dir, "requests.jsonl"), maxSize: 100 << 20, maxBackups: 10, maxAge: 14 * 24 * time.Hour, mode: 0o600}
	if err := writer.open(); err != nil {
		return nil, nil, err
	}
	encoder := zap.NewProductionEncoderConfig()
	encoder.EncodeTime = zapcore.ISO8601TimeEncoder
	log := zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(encoder), writer, zap.InfoLevel), zap.ErrorOutput(zapcore.Lock(os.Stderr)))
	return log, func() { _ = log.Sync(); _ = writer.Close() }, nil
}
