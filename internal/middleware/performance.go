package middleware

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/truewhile/MeBox/internal/perftrace"
)

// PerformanceTrace must precede Recovery so recovered status codes are logged.
// It emits one bounded record, never request bodies, headers, URL queries or IDs
// supplied by clients. The existing private compatibility sink handles rotation.
func PerformanceTrace(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if trace := perftrace.From(ctx); trace != nil {
			// Gin's normalized Emby redispatch resets Writer, but retains Request.
			if _, wrapped := c.Writer.(*performanceWriter); !wrapped {
				c.Writer = &performanceWriter{ResponseWriter: c.Writer, ctx: ctx, started: trace.StartedAt()}
			}
			c.Next()
			return
		}
		ctx, trace := perftrace.New(ctx)
		started := trace.StartedAt()
		c.Request = c.Request.WithContext(ctx)
		if c.Request.Body != nil && c.Request.Body != http.NoBody {
			c.Request.Body = &performanceBody{ReadCloser: c.Request.Body, ctx: ctx}
		}
		writer := &performanceWriter{ResponseWriter: c.Writer, ctx: ctx, started: started}
		c.Writer = writer
		c.Header("X-Request-ID", trace.ID)
		defer func() {
			route := c.FullPath()
			if route == "" {
				route = "<unmatched>"
			}
			log.Info("performance",
				zap.String("method", c.Request.Method),
				zap.String("route", route),
				zap.Int("status", c.Writer.Status()),
				zap.Int("response_bytes", max(0, c.Writer.Size())),
				zap.Bool("range_requested", c.GetHeader("Range") != ""),
				zap.Bool("cancelled", ctx.Err() != nil),
				zap.Any("trace", trace.Finish()),
			)
		}()
		c.Next()
	}
}

type performanceWriter struct {
	gin.ResponseWriter
	ctx     context.Context
	started time.Time
	ready   bool
}

func (w *performanceWriter) markReady() {
	if !w.ready {
		w.ready = true
		perftrace.Record(w.ctx, "http.response_ready", w.started, 0)
	}
}

func (w *performanceWriter) WriteHeaderNow() {
	w.markReady()
	w.ResponseWriter.WriteHeaderNow()
}

func (w *performanceWriter) Write(p []byte) (int, error) {
	w.markReady()
	start := time.Now()
	n, err := w.ResponseWriter.Write(p)
	perftrace.Record(w.ctx, "http.write", start, int64(n))
	if err != nil {
		perftrace.Count(w.ctx, "http.write_error")
	}
	return n, err
}

func (w *performanceWriter) WriteString(s string) (int, error) {
	w.markReady()
	start := time.Now()
	n, err := w.ResponseWriter.WriteString(s)
	perftrace.Record(w.ctx, "http.write", start, int64(n))
	if err != nil {
		perftrace.Count(w.ctx, "http.write_error")
	}
	return n, err
}

func (w *performanceWriter) Flush() {
	w.markReady()
	start := time.Now()
	w.ResponseWriter.Flush()
	perftrace.Record(w.ctx, "http.flush", start, 0)
}

func (w *performanceWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

type performanceBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *performanceBody) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := b.ReadCloser.Read(p)
	perftrace.Record(b.ctx, "http.request_read", start, int64(n))
	if err != nil && err != io.EOF {
		perftrace.Count(b.ctx, "http.request_read_error")
	}
	return n, err
}
