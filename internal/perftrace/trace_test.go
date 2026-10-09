package perftrace

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestTraceBoundsConcurrentMetricsAndFreezesAfterFinish(t *testing.T) {
	ctx, trace := New(context.Background())
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 100 {
				Record(ctx, "stream.read", time.Now().Add(-slowThreshold), 4)
			}
		}()
	}
	workers.Wait()
	for i := range maxMetrics + 5 {
		Count(ctx, fmt.Sprintf("stage.%d", i))
	}
	snapshot := trace.Finish()
	if len(snapshot.Metrics) != maxMetrics || snapshot.DroppedMetrics != 6 || len(snapshot.Slow) != maxSlowEvents || snapshot.DroppedEvents != 800-maxSlowEvents {
		t.Fatalf("bounds: metrics=%d dropped=%d slow=%d dropped=%d", len(snapshot.Metrics), snapshot.DroppedMetrics, len(snapshot.Slow), snapshot.DroppedEvents)
	}
	metric := snapshot.Metrics[0]
	if metric.Name != "stream.read" || metric.Count != 800 || metric.Bytes != 3200 || metric.TotalNS < int64(800*slowThreshold) || metric.MaxNS < int64(slowThreshold) {
		t.Fatalf("concurrent aggregate = %+v", metric)
	}
	Count(ctx, "stream.read")
	if trace.Finish().Metrics[0].Count != 800 || snapshot.Metrics[0].Count != 800 {
		t.Fatal("completed snapshot mutated")
	}
}

func TestDetachedTraceDoesNotRetainIdentityOrCancellation(t *testing.T) {
	type identityKey struct{}
	parent, cancel := context.WithCancel(context.WithValue(context.Background(), identityKey{}, "private-user"))
	ctx, trace := New(parent)
	detached := Detach(ctx)
	cancel()
	if detached.Err() != nil || detached.Value(identityKey{}) != nil {
		t.Fatal("background trace inherited request state")
	}
	Count(detached, "hls.ffmpeg.start")
	if trace.Finish().Metrics[0].Count != 1 {
		t.Fatal("detached start was not correlated")
	}
}

func TestHTTPTraceMeasuresRealTLSConnectionReuseAndBody(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "media-bytes") }))
	defer srv.Close()
	client := srv.Client()
	ctx, trace := New(context.Background())
	for range 2 {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/?token=private-token", nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := client.Do(Request(req))
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(Body(req, resp.Body))
		_ = resp.Body.Close()
		if err != nil || string(body) != "media-bytes" {
			t.Fatalf("upstream bytes: %q, %v", body, err)
		}
	}
	snapshot := trace.Finish()
	counts := make(map[string]int64)
	var read int64
	for _, metric := range snapshot.Metrics {
		counts[metric.Name] = metric.Count
		if metric.Name == "upstream.read" {
			read += metric.Bytes
		}
	}
	for name, expected := range map[string]int64{"upstream.connection.new": 1, "upstream.connection.reused": 1, "upstream.connect": 1, "upstream.tls": 1, "upstream.first_byte": 2} {
		if counts[name] != expected {
			t.Fatalf("%s count=%d want=%d", name, counts[name], expected)
		}
	}
	if read != 22 {
		t.Fatalf("upstream bytes=%d", read)
	}
}
