package perftrace

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptrace"
	"sync"
	"time"
)

// Request attaches the standard-library transport hooks only to traced
// contexts. Addresses, headers and response bodies never enter the record.
func Request(req *http.Request) *http.Request {
	ctx := req.Context()
	if From(ctx) == nil {
		return req
	}
	started := time.Now()
	var mu sync.Mutex
	var acquire, dns, handshake time.Time
	type connectKey struct{ network, address string }
	var connects map[connectKey]time.Time
	hooks := &httptrace.ClientTrace{
		GetConn: func(string) { mu.Lock(); acquire = time.Now(); mu.Unlock() },
		GotConn: func(info httptrace.GotConnInfo) {
			mu.Lock()
			begin := acquire
			mu.Unlock()
			Record(ctx, "upstream.connection_acquire", begin, 0)
			if info.Reused {
				Count(ctx, "upstream.connection.reused")
			} else {
				Count(ctx, "upstream.connection.new")
			}
		},
		DNSStart: func(httptrace.DNSStartInfo) { mu.Lock(); dns = time.Now(); mu.Unlock() },
		DNSDone: func(info httptrace.DNSDoneInfo) {
			mu.Lock()
			begin := dns
			mu.Unlock()
			Record(ctx, "upstream.dns", begin, 0)
			if info.Err != nil {
				Count(ctx, "upstream.dns_error")
			}
		},
		ConnectStart: func(network, address string) {
			mu.Lock()
			defer mu.Unlock()
			if connects == nil {
				connects = make(map[connectKey]time.Time)
			}
			connects[connectKey{network, address}] = time.Now()
		},
		ConnectDone: func(network, address string, err error) {
			mu.Lock()
			key := connectKey{network, address}
			begin := connects[key]
			delete(connects, key)
			mu.Unlock()
			Record(ctx, "upstream.connect", begin, 0)
			if err != nil {
				Count(ctx, "upstream.connect_error")
			}
		},
		TLSHandshakeStart: func() { mu.Lock(); handshake = time.Now(); mu.Unlock() },
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			mu.Lock()
			begin := handshake
			mu.Unlock()
			Record(ctx, "upstream.tls", begin, 0)
			if err != nil {
				Count(ctx, "upstream.tls_error")
			}
		},
		GotFirstResponseByte: func() { Record(ctx, "upstream.first_byte", started, 0) },
	}
	return req.WithContext(httptrace.WithClientTrace(ctx, hooks))
}

// Body separates upstream reads from downstream writes during stream proxying.
func Body(req *http.Request, body io.ReadCloser) io.ReadCloser {
	if From(req.Context()) == nil {
		return body
	}
	return &timedBody{ReadCloser: body, ctx: req.Context()}
}

type timedBody struct {
	io.ReadCloser
	ctx context.Context
}

func (b *timedBody) Read(p []byte) (int, error) {
	start := time.Now()
	n, err := b.ReadCloser.Read(p)
	Record(b.ctx, "upstream.read", start, int64(n))
	if err != nil && err != io.EOF {
		Count(b.ctx, "upstream.read_error")
	}
	return n, err
}
