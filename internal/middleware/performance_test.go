package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/truewhile/MeBox/internal/perftrace"
)

func TestPerformanceTracePreservesRangeAndRedactsRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(PerformanceTrace(zap.New(core)))
	content := []byte("0123456789abcdefghijklmnop")
	r.GET("/videos/:id", func(c *gin.Context) {
		http.ServeContent(c.Writer, c.Request, "video.mp4", time.Time{}, perftrace.Reader(c.Request.Context(), bytes.NewReader(content)))
	})
	req := httptest.NewRequest(http.MethodGet, "/videos/private-media-id?token=secret-token", nil)
	req.Header.Set("Range", "bytes=5-12")
	req.Header.Set("Authorization", "Bearer secret-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || w.Body.String() != "56789abc" || w.Header().Get("Content-Range") != "bytes 5-12/26" {
		t.Fatalf("ranged response = %d %q %q", w.Code, w.Body.String(), w.Header().Get("Content-Range"))
	}
	entry := logs.All()[0].ContextMap()
	trace := entry["trace"].(perftrace.Snapshot)
	if w.Header().Get("X-Request-ID") != trace.ID || entry["route"] != "/videos/:id" || entry["response_bytes"] != int64(8) {
		t.Fatalf("wrong correlation/route/size: %+v", entry)
	}
	var written int64
	var ready int64
	for _, metric := range trace.Metrics {
		if metric.Name == "http.write" {
			written += metric.Bytes
		}
		if metric.Name == "http.response_ready" {
			ready += metric.Count
		}
	}
	if written != 8 || ready != 1 {
		t.Fatalf("write/ready metrics = %d/%d", written, ready)
	}
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"secret-token", "private-media-id", "Bearer"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatalf("trace exposed %q", secret)
		}
	}
}

func TestPerformanceTraceRecoveryAndBodyRead(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(PerformanceTrace(zap.New(core)), gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) { c.AbortWithStatus(http.StatusInternalServerError) }))
	r.POST("/input", func(c *gin.Context) {
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(data) != "private-input" {
			t.Fatalf("body altered: %q", data)
		}
		panic("test")
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/input", strings.NewReader("private-input")))
	entry := logs.All()[0].ContextMap()
	if w.Code != 500 || entry["status"] != int64(500) {
		t.Fatalf("recovered status = %d, log=%v", w.Code, entry["status"])
	}
	trace := entry["trace"].(perftrace.Snapshot)
	var read int64
	for _, metric := range trace.Metrics {
		if metric.Name == "http.request_read" {
			read += metric.Bytes
		}
	}
	if read != 13 {
		t.Fatalf("request read bytes = %d", read)
	}
	data, _ := json.Marshal(entry)
	if bytes.Contains(data, []byte("private-input")) {
		t.Fatal("body leaked into trace")
	}
}

func BenchmarkPerformanceTrace(b *testing.B) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{false, true} {
		name := "disabled"
		if enabled {
			name = "enabled"
		}
		b.Run(name, func(b *testing.B) {
			r := gin.New()
			if enabled {
				r.Use(PerformanceTrace(zap.New(zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), zapcore.AddSync(io.Discard), zap.InfoLevel))))
			}
			r.GET("/playback", func(c *gin.Context) {
				for _, stage := range []string{"db.sql", "media.lookup", "media.sources", "subtitle.discover", "upstream.first_byte"} {
					started := perftrace.Begin(c.Request.Context())
					perftrace.End(c.Request.Context(), stage, started)
				}
				c.String(http.StatusOK, "playback")
			})
			req := httptest.NewRequest(http.MethodGet, "/playback", nil)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				r.ServeHTTP(httptest.NewRecorder(), req)
			}
		})
	}
}
