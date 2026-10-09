// Package perftrace records bounded, request-scoped timing without logging
// credentials, SQL parameters, media paths or upstream URLs.
package perftrace

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	maxMetrics    = 64
	maxSlowEvents = 32
	slowThreshold = 100 * time.Millisecond
)

type contextKey struct{}

type Metric struct {
	Name    string `json:"name"`
	Count   int64  `json:"count"`
	TotalNS int64  `json:"total_ns"`
	MaxNS   int64  `json:"max_ns"`
	Bytes   int64  `json:"bytes,omitempty"`
}

type Event struct {
	Name       string `json:"name"`
	OffsetNS   int64  `json:"offset_ns"`
	DurationNS int64  `json:"duration_ns"`
}

type Snapshot struct {
	ID             string   `json:"id"`
	ElapsedNS      int64    `json:"elapsed_ns"`
	Metrics        []Metric `json:"metrics"`
	Slow           []Event  `json:"slow,omitempty"`
	DroppedMetrics int64    `json:"dropped_metrics,omitempty"`
	DroppedEvents  int64    `json:"dropped_events,omitempty"`
}

type Trace struct {
	ID             string
	started        time.Time
	mu             sync.Mutex
	closed         bool
	metrics        []Metric
	slow           []Event
	droppedMetrics int64
	droppedEvents  int64
}

func New(ctx context.Context) (context.Context, *Trace) {
	t := &Trace{ID: uuid.NewString(), started: time.Now()}
	return context.WithValue(ctx, contextKey{}, t), t
}

func From(ctx context.Context) *Trace {
	if ctx == nil {
		return nil
	}
	t, _ := ctx.Value(contextKey{}).(*Trace)
	return t
}

func (t *Trace) StartedAt() time.Time { return t.started }

// Detach retains only the bounded trace, not HTTP body, identity values or
// cancellation. Background jobs must keep their existing lifecycle context.
func Detach(ctx context.Context) context.Context {
	if trace := From(ctx); trace != nil {
		return context.WithValue(context.Background(), contextKey{}, trace)
	}
	return context.Background()
}

// Begin returns zero without reading the clock when the request is not traced.
func Begin(ctx context.Context) time.Time {
	if From(ctx) == nil {
		return time.Time{}
	}
	return time.Now()
}

func End(ctx context.Context, name string, started time.Time) {
	if !started.IsZero() {
		Record(ctx, name, started, 0)
	}
}

// Record uses fixed call-site names; callers must not include arbitrary IDs,
// SQL, filenames or URL values. Timings are inclusive and can overlap.
func Record(ctx context.Context, name string, started time.Time, bytes int64) {
	t := From(ctx)
	if t == nil || started.IsZero() {
		return
	}
	t.record(name, started, time.Since(started), bytes)
}

// Count records cache decisions and other discrete events without a timer.
func Count(ctx context.Context, name string) {
	if t := From(ctx); t != nil {
		t.record(name, time.Time{}, 0, 0)
	}
}

func (t *Trace) record(name string, started time.Time, duration time.Duration, bytes int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	index := -1
	for i := range t.metrics {
		if t.metrics[i].Name == name {
			index = i
			break
		}
	}
	if index < 0 {
		if len(t.metrics) >= maxMetrics {
			t.droppedMetrics++
			return
		}
		t.metrics = append(t.metrics, Metric{Name: name})
		index = len(t.metrics) - 1
	}
	m := &t.metrics[index]
	m.Count++
	m.TotalNS += int64(duration)
	if int64(duration) > m.MaxNS {
		m.MaxNS = int64(duration)
	}
	m.Bytes += bytes
	if duration >= slowThreshold {
		if len(t.slow) >= maxSlowEvents {
			t.droppedEvents++
			return
		}
		t.slow = append(t.slow, Event{Name: name, OffsetNS: int64(started.Sub(t.started)), DurationNS: int64(duration)})
	}
}

// Finish freezes the record; detached background work cannot mutate a logged
// snapshot or retain an unbounded sequence after the HTTP request completes.
func (t *Trace) Finish() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	return Snapshot{ID: t.ID, ElapsedNS: int64(time.Since(t.started)), Metrics: t.metrics, Slow: t.slow, DroppedMetrics: t.droppedMetrics, DroppedEvents: t.droppedEvents}
}

// Reader preserves the original object when tracing is disabled. Enabled
// streaming aggregates read/seek durations, rather than logging every chunk.
func Reader(ctx context.Context, source io.ReadSeeker) io.ReadSeeker {
	if From(ctx) == nil {
		return source
	}
	return &timedReader{ctx: ctx, ReadSeeker: source}
}

type timedReader struct {
	io.ReadSeeker
	ctx context.Context
}

func (r *timedReader) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := r.ReadSeeker.Read(p)
	Record(r.ctx, "stream.read", start, int64(n))
	return n, err
}

func (r *timedReader) Seek(offset int64, whence int) (int64, error) {
	start := time.Now()
	position, err := r.ReadSeeker.Seek(offset, whence)
	Record(r.ctx, "stream.seek", start, 0)
	return position, err
}
