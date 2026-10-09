package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/truewhile/MeBox/internal/handler"
	"github.com/truewhile/MeBox/internal/middleware"
	"github.com/truewhile/MeBox/internal/perftrace"
)

func TestPerformanceTraceNormalizedEmbyKeepsOneCorrelatedRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(middleware.PerformanceTrace(zap.New(core)))
	r.GET("/emby/system/info/public", func(c *gin.Context) { c.String(http.StatusOK, "server-info") })
	r.NoRoute(func(c *gin.Context) {
		if !handler.TryHandleEmbyNormalizedRoute(c, r) {
			c.Status(http.StatusNotFound)
		}
	})
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/emby/emby/system/info/public", nil))
	if w.Code != http.StatusOK || w.Body.String() != "server-info" {
		t.Fatalf("normalized response=%d %q", w.Code, w.Body.String())
	}
	entries := logs.FilterMessage("performance").All()
	if len(entries) != 1 {
		t.Fatalf("one HTTP request produced %d independent traces", len(entries))
	}
	fields := entries[0].ContextMap()
	trace := fields["trace"].(perftrace.Snapshot)
	if w.Header().Get("X-Request-ID") != trace.ID || fields["route"] != "/emby/system/info/public" {
		t.Fatalf("correlation/route mismatch: %+v", fields)
	}
	var bytes int64
	var ready int64
	for _, metric := range trace.Metrics {
		if metric.Name == "http.write" {
			bytes += metric.Bytes
		}
		if metric.Name == "http.response_ready" {
			ready += metric.Count
		}
	}
	if bytes != 11 || ready != 1 {
		t.Fatalf("normalized response bytes/ready=%d/%d", bytes, ready)
	}
}
