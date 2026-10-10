package middleware

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/truewhile/MeBox/internal/model"
	"github.com/truewhile/MeBox/internal/perftrace"
	"github.com/truewhile/MeBox/internal/repository"
)

func detailedRequestTestToken(t *testing.T, userID string) string {
	t.Helper()
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{UserID: userID, Role: "user"}).SignedString([]byte("details-test-secret"))
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func TestDetailedRequestsAuthenticatedIdentityAndFidelity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(DetailedRequests(zap.New(core), nil))
	r.POST("/users/:id/playback", AuthRequired("details-test-secret"), detailedRequestTestUserName, func(c *gin.Context) {
		data, err := io.ReadAll(c.Request.Body)
		if err != nil || !bytes.Contains(data, []byte(`"AccessToken":"body-secret"`)) {
			t.Fatalf("handler received altered body: %q, %v", data, err)
		}
		c.Header("Location", "https://media.example/play?token=response-secret&MediaSourceId=source-1")
		c.Status(http.StatusAccepted)
	})
	r.GET("/health", func(c *gin.Context) { c.String(http.StatusOK, "healthy") })
	payload := `{"ItemId":"media-123","PositionTicks":9007199254740993,"Enabled":true,"Rate":1.25,"SubtitleStreamIndex":-1,"Null":null,"AccessToken":"body-secret","DeviceProfile":{"Name":"tv","DirectPlayProfiles":[{"Type":"Video","Container":"mp4"}]},"PlaySessionId":"session-1","ProfileId":"profile-1","MediaSourceId":"source-1"}`
	var tokens []string
	for _, userID := range []string{"user-a", "user-b"} {
		token := detailedRequestTestToken(t, userID)
		tokens = append(tokens, token)
		req := httptest.NewRequest(http.MethodPost, "/users/media-123/playback?UserId=pretender&Fields=Path&Fields=MediaSources&token=query-secret", strings.NewReader(payload))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-Emby-Authorization", `MediaBrowser Client="Living Room", Device="TV", DeviceId="device-1", Version="2.3", Token="emby-secret"`)
		req.Header.Set("Cookie", "mebox_access_token=cookie-secret")
		req.Header.Set("Range", "bytes=5-12")
		req.Header.Set("X-Request-ID", "client-controlled-id")
		req.Header.Set("Content-Type", "application/json; charset=utf-8")
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		if w.Code != http.StatusAccepted {
			t.Fatalf("status = %d", w.Code)
		}
		entry := logs.FilterMessage("request_details").All()[len(tokens)-1].ContextMap()
		if w.Header().Get("X-Request-ID") != entry["request_id"] {
			t.Fatal("client-visible request ID does not correlate with detailed log")
		}
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/health?UserId=pretender", nil))
	entries := logs.FilterMessage("request_details").All()
	if len(entries) != 3 {
		t.Fatalf("entry count = %d", len(entries))
	}
	ids := make(map[string]bool)
	for i, entry := range entries {
		fields := entry.ContextMap()
		id := fields["request_id"].(string)
		if _, err := uuid.Parse(id); err != nil || ids[id] || id == "client-controlled-id" {
			t.Fatalf("not a distinct server request ID: %q", id)
		}
		ids[id] = true
		if fields["claimed_user_id"] != "pretender" || fields["started_at"].(time.Time).IsZero() || fields["duration"].(time.Duration) < 0 {
			t.Fatalf("missing identity/start/duration: %+v", fields)
		}
		if i == 2 {
			if fields["user_id"] != "" || fields["username"] != "" || fields["username_state"] != "anonymous" || fields["anonymous"] != true || fields["body_state"] != "empty" || fields["status"] != int64(http.StatusOK) || fields["response_bytes"] != int64(7) {
				t.Fatalf("anonymous health entry = %+v", fields)
			}
			continue
		}
		if fields["user_id"] != []string{"user-a", "user-b"}[i] || fields["username"] != "name-"+[]string{"user-a", "user-b"}[i] || fields["username_state"] != "known" || fields["anonymous"] != false || fields["status"] != int64(http.StatusAccepted) || fields["route"] != "/users/:id/playback" {
			t.Fatalf("authenticated entry = %+v", fields)
		}
		query := fields["query"].(url.Values)
		headers := fields["headers"].(http.Header)
		if strings.Join(query["Fields"], ",") != "Path,MediaSources" || headers.Get("Range") != "bytes=5-12" {
			t.Fatalf("lost query/header business fields: %v, %v", query, headers)
		}
		body := fields["body"].(map[string]any)
		if fields["body_state"] != "complete" || fields["body_read_bytes"] != int64(len(payload)) || fields["content_length"] != int64(len(payload)) ||
			body["PositionTicks"] != json.Number("9007199254740993") || body["Enabled"] != true || body["Rate"] != json.Number("1.25") || body["SubtitleStreamIndex"] != json.Number("-1") || body["Null"] != nil || body["ItemId"] != "media-123" {
			t.Fatalf("body types/values changed: %+v", fields)
		}
		if body["DeviceProfile"].(map[string]any)["DirectPlayProfiles"].([]any)[0].(map[string]any)["Container"] != "mp4" || fields["client"] != "Living Room" || fields["device_id"] != "device-1" || fields["session_id"] != "session-1" || fields["profile_id"] != "profile-1" || fields["requested_media_source_id"] != "source-1" {
			t.Fatalf("missing capabilities/client/playback identity: %+v", fields)
		}
		serialized, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range append(tokens, "query-secret", "body-secret", "emby-secret", "cookie-secret", "response-secret") {
			if bytes.Contains(serialized, []byte(secret)) {
				t.Fatalf("credential leaked: %s", secret)
			}
		}
	}
}

func TestDetailedRequestsRedispatchCapturesOriginalInputsOnce(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(PerformanceTrace(zap.New(core)), DetailedRequests(zap.New(core), nil))
	r.POST("/users/:id", AuthRequired("details-test-secret"), detailedRequestTestUserName, func(c *gin.Context) {
		if _, err := io.ReadAll(c.Request.Body); err != nil {
			t.Fatal(err)
		}
		c.String(http.StatusCreated, "normalized")
	})
	r.NoRoute(func(c *gin.Context) {
		c.Set("discarded_gin_key", true)
		c.Request.URL.Path = "/users/Mixed-ID"
		c.Request.URL.RawQuery = "Fields=Changed"
		c.Request.Header.Set("Range", "bytes=0-1")
		r.HandleContext(c)
	})
	req := httptest.NewRequest(http.MethodPost, "/Users/Mixed-ID?Fields=Original", strings.NewReader(`{"ItemId":"Mixed-ID"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Range", "bytes=5-12")
	req.Header.Set("Authorization", "Bearer "+detailedRequestTestToken(t, "normalized-user"))
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	entries := logs.FilterMessage("request_details").All()
	if len(entries) != 1 || w.Code != http.StatusCreated || w.Body.String() != "normalized" {
		t.Fatalf("duplicate or changed response: %d entries, status %d, body %q", len(entries), w.Code, w.Body.String())
	}
	fields := entries[0].ContextMap()
	if fields["path"] != "/Users/Mixed-ID" || fields["normalized_path"] != "/users/Mixed-ID" || fields["route"] != "/users/:id" || fields["user_id"] != "normalized-user" || fields["username"] != "name-normalized-user" || fields["username_state"] != "known" || fields["query"].(url.Values).Get("Fields") != "Original" || fields["headers"].(http.Header).Get("Range") != "bytes=5-12" || fields["body_state"] != "complete" {
		t.Fatalf("redispatch lost original input or final identity: %+v", fields)
	}
	performanceEntries := logs.FilterMessage("performance").All()
	if len(performanceEntries) != 1 {
		t.Fatalf("performance entry count = %d", len(performanceEntries))
	}
	trace := performanceEntries[0].ContextMap()["trace"].(perftrace.Snapshot)
	if fields["request_id"] != trace.ID || w.Header().Get("X-Request-ID") != trace.ID {
		t.Fatalf("request/trace ID mismatch: %+v, %+v", fields, trace)
	}
	var readBytes int64
	for _, metric := range trace.Metrics {
		if metric.Name == "http.request_read" {
			readBytes += metric.Bytes
		}
	}
	if readBytes != int64(len(`{"ItemId":"Mixed-ID"}`)) {
		t.Fatalf("request tap changed tracing: %d", readBytes)
	}
}

type detailedErrorBody struct {
	data     string
	err      error
	reads    int
	closed   bool
	closeErr error
}

func (b *detailedErrorBody) Read(p []byte) (int, error) {
	b.reads++
	n := copy(p, b.data)
	b.data = b.data[n:]
	return n, b.err
}

func (b *detailedErrorBody) Close() error {
	b.closed = true
	return b.closeErr
}

func TestDetailedRequestsPreservesReadAndCloseErrors(t *testing.T) {
	gin.SetMode(gin.TestMode)
	readErr := errors.New("read failed with secret-error")
	closeErr := errors.New("close failed")
	original := &detailedErrorBody{data: `{"AccessToken":"partial-secret"`, err: readErr, closeErr: closeErr}
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(DetailedRequests(zap.New(core), nil))
	r.POST("/read", func(c *gin.Context) {
		if original.reads != 0 {
			t.Fatal("middleware pre-read body")
		}
		buf := make([]byte, 128)
		n, err := c.Request.Body.Read(buf)
		if n != len(`{"AccessToken":"partial-secret"`) || err != readErr || string(buf[:n]) != `{"AccessToken":"partial-secret"` {
			t.Fatalf("n+error changed: %d %v %q", n, err, buf[:n])
		}
		if err := c.Request.Body.Close(); err != closeErr || !original.closed {
			t.Fatalf("close not forwarded: %v", err)
		}
		c.Status(http.StatusBadRequest)
	})
	req := httptest.NewRequest(http.MethodPost, "/read", nil)
	req.Body = original
	req.ContentLength = -1
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(httptest.NewRecorder(), req)
	fields := logs.All()[0].ContextMap()
	if fields["body_state"] != "incomplete" || fields["body"] != nil || fields["body_read_error"] != true || original.reads != 1 {
		t.Fatalf("unsafe partial body or extra reads: %+v, reads=%d", fields, original.reads)
	}
	data, _ := json.Marshal(fields)
	if bytes.Contains(data, []byte("partial-secret")) || bytes.Contains(data, []byte("secret-error")) {
		t.Fatal("raw partial body/error text leaked")
	}
}

func TestDetailedRequestsBodyStatesAndOversizedForwarding(t *testing.T) {
	gin.SetMode(gin.TestMode)
	oversized := `{"Value":"` + strings.Repeat("x", detailedRequestBodyLimit) + `"}`
	for _, test := range []struct {
		name        string
		contentType string
		encoding    string
		body        string
		read        string
		length      int64
		state       string
	}{
		{name: "unread", contentType: "application/json", body: `{"Value":"unread-secret"}`, read: "none", length: -1, state: "unread"},
		{name: "partial", contentType: "application/json", body: `{"Value":"partial-secret"}`, read: "partial", length: -1, state: "incomplete"},
		{name: "malformed", contentType: "application/json", body: `{"Value":"malformed-secret"`, read: "all", length: -1, state: "invalid_json"},
		{name: "binary", contentType: "application/octet-stream", body: "binary-secret", read: "all", length: -1, state: "unsupported"},
		{name: "multipart", contentType: "multipart/form-data; boundary=abc", body: "multipart-secret", read: "all", length: -1, state: "unsupported"},
		{name: "encoded", contentType: "application/json", encoding: "gzip", body: "compressed-secret", read: "all", length: -1, state: "unsupported"},
		{name: "oversized_unknown", contentType: "application/json", body: oversized, read: "all", length: -1, state: "truncated"},
		{name: "oversized_known", contentType: "application/json", body: oversized, read: "all", length: int64(len(oversized)), state: "truncated"},
	} {
		t.Run(test.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			r := gin.New()
			r.Use(DetailedRequests(zap.New(core), nil))
			var expectedRead int64
			r.POST("/input", func(c *gin.Context) {
				switch test.read {
				case "all":
					data, err := io.ReadAll(c.Request.Body)
					if err != nil || string(data) != test.body {
						t.Fatalf("upload changed: %d bytes, %v", len(data), err)
					}
					expectedRead = int64(len(data))
				case "partial":
					p := make([]byte, 3)
					n, err := c.Request.Body.Read(p)
					if n != 3 || err != nil {
						t.Fatalf("partial read = %d, %v", n, err)
					}
					expectedRead = int64(n)
				}
				tap := c.Request.Body.(*detailedRequestBody)
				if cap(tap.data) > detailedRequestBodyLimit || (!tap.supported && len(tap.data) != 0) {
					t.Fatalf("unbounded/binary capture: capacity=%d, length=%d", cap(tap.data), len(tap.data))
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/input", strings.NewReader(test.body))
			req.ContentLength = test.length
			req.Header.Set("Content-Type", test.contentType)
			req.Header.Set("Content-Encoding", test.encoding)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)
			fields := logs.All()[0].ContextMap()
			if w.Code != http.StatusNoContent || fields["body_state"] != test.state || fields["body"] != nil || fields["body_read_bytes"] != expectedRead {
				t.Fatalf("body state/forwarding mismatch: %+v", fields)
			}
		})
	}
}

func TestDetailedRequestsRecoveryCancellationAndRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(DetailedRequests(zap.New(core), nil), gin.CustomRecoveryWithWriter(io.Discard, func(c *gin.Context, _ any) { c.AbortWithStatus(http.StatusInternalServerError) }))
	r.GET("/panic", func(c *gin.Context) { panic("secret-panic") })
	r.GET("/video/:id", func(c *gin.Context) {
		http.ServeContent(c.Writer, c.Request, "video.mp4", time.Time{}, strings.NewReader("0123456789abcdefghijklmnop"))
	})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/panic", nil).WithContext(ctx))
	fields := logs.All()[0].ContextMap()
	if w.Code != http.StatusInternalServerError || fields["status"] != int64(http.StatusInternalServerError) || fields["cancelled"] != true {
		t.Fatalf("recovery/cancellation state lost: %+v", fields)
	}
	req := httptest.NewRequest(http.MethodGet, "/video/media-id", nil)
	req.Header.Set("Range", "bytes=5-12")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	fields = logs.All()[1].ContextMap()
	if w.Code != http.StatusPartialContent || w.Body.String() != "56789abc" || w.Header().Get("Content-Range") != "bytes 5-12/26" || fields["status"] != int64(http.StatusPartialContent) || fields["response_bytes"] != int64(8) || fields["headers"].(http.Header).Get("Range") != "bytes=5-12" {
		t.Fatalf("Range response changed: response=%d %q, fields=%+v", w.Code, w.Body.String(), fields)
	}
	if fields["response_content_range"] != "bytes 5-12/26" || fields["response_content_length"] != "8" || fields["response_content_type"] != "video/mp4" || fields["response_headers"].(http.Header).Get("Content-Range") != "bytes 5-12/26" {
		t.Fatalf("Range response metadata lost: %+v", fields)
	}
	for _, entry := range logs.All() {
		data, _ := json.Marshal(entry.ContextMap())
		if bytes.Contains(data, []byte("secret-panic")) || bytes.Contains(data, []byte("56789abc")) {
			t.Fatal("panic or response body leaked")
		}
	}
}

func TestDetailedRequestsBodyCompletionBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name        string
		contentType string
		data        string
		length      int64
		err         error
		state       string
	}{
		{name: "bytes_and_eof", contentType: "application/json", data: `{"Value":true}`, length: -1, err: io.EOF, state: "complete"},
		{name: "known_length_without_eof", contentType: "application/vnd.emby+json", data: `{"Value":true}`, length: int64(len(`{"Value":true}`)), state: "complete"},
		{name: "short_declared_length", contentType: "application/json", data: `{"Value":true}`, length: 100, err: io.EOF, state: "incomplete"},
		{name: "form", contentType: "application/x-www-form-urlencoded", data: "ItemId=item-1&PlaySessionId=session-1&Password=form-secret", length: -1, err: io.EOF, state: "complete"},
		{name: "text_json", contentType: "text/plain; charset=UTF-8", data: `{"PositionTicks":9007199254740993,"MediaSourceId":"item-1:mp4","PlaySessionId":"session-1","AccessToken":"text-secret"}`, length: -1, err: io.EOF, state: "complete"},
		{name: "plain_text", contentType: "text/plain", data: "plain-secret", length: -1, err: io.EOF, state: "unsupported"},
		{name: "plain_json_scalar", contentType: "text/plain", data: `"scalar-secret"`, length: -1, err: io.EOF, state: "unsupported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			original := &detailedErrorBody{data: test.data, err: test.err}
			r := gin.New()
			r.Use(DetailedRequests(zap.New(core), nil))
			r.POST("/body", func(c *gin.Context) {
				buf := make([]byte, len(test.data)+1)
				n, err := c.Request.Body.Read(buf)
				if n != len(test.data) || err != test.err || string(buf[:n]) != test.data {
					t.Fatalf("read altered: %d %v %q", n, err, buf[:n])
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/body", nil)
			req.Body, req.ContentLength = original, test.length
			req.Header.Set("Content-Type", test.contentType)
			r.ServeHTTP(httptest.NewRecorder(), req)
			fields := logs.All()[0].ContextMap()
			if fields["body_state"] != test.state || original.reads != 1 {
				t.Fatalf("wrong completeness or hidden drain: %+v, reads=%d", fields, original.reads)
			}
			if test.name == "form" {
				form := fields["body"].(url.Values)
				serialized, _ := json.Marshal(fields)
				if form.Get("ItemId") != "item-1" || fields["session_id"] != "session-1" || bytes.Contains(serialized, []byte("form-secret")) {
					t.Fatalf("form fields lost or credential exposed: %+v", fields)
				}
			}
			if test.name == "text_json" {
				body := fields["body"].(map[string]any)
				if body["PositionTicks"] != json.Number("9007199254740993") || fields["requested_media_source_id"] != "item-1:mp4" || fields["session_id"] != "session-1" || body["AccessToken"] != "[REDACTED]" {
					t.Fatalf("plain JSON playback fields lost or credential exposed: %+v", fields)
				}
			}
		})
	}
}

func TestDetailedRequestsLogsFailuresAndStaticRequests(t *testing.T) {
	gin.SetMode(gin.TestMode)
	core, logs := observer.New(zap.InfoLevel)
	r := gin.New()
	r.Use(DetailedRequests(zap.New(core), nil))
	r.GET("/private", AuthRequired("details-test-secret"), func(c *gin.Context) { t.Fatal("unauthenticated handler reached") })
	r.GET("/assets/app.js", func(c *gin.Context) { c.Data(http.StatusOK, "text/javascript", []byte("static-content")) })
	for _, test := range []struct {
		path   string
		status int
		route  string
	}{
		{path: "/private?UserId=pretender&token=invalid-secret", status: http.StatusUnauthorized, route: "/private"},
		{path: "/missing", status: http.StatusNotFound, route: "<unmatched>"},
		{path: "/assets/app.js", status: http.StatusOK, route: "/assets/app.js"},
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, test.path, nil))
		entries := logs.All()
		fields := entries[len(entries)-1].ContextMap()
		if w.Code != test.status || fields["status"] != int64(test.status) || fields["route"] != test.route || fields["anonymous"] != true || fields["user_id"] != "" {
			t.Fatalf("failure/static entry = %+v", fields)
		}
		data, _ := json.Marshal(fields)
		if bytes.Contains(data, []byte("invalid-secret")) || bytes.Contains(data, []byte("static-content")) {
			t.Fatal("credential or static response body exposed")
		}
	}
	if logs.Len() != 3 {
		t.Fatalf("entry count = %d", logs.Len())
	}
}

func detailedRequestTestUserName(c *gin.Context) {
	if userID := c.GetString(CtxUserID); userID != "" {
		c.Set(CtxUserName, "name-"+userID)
	}
	c.Next()
}

func TestDetailedRequestsUsernameRepositoryFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })
	if err := db.AutoMigrate(&model.User{}); err != nil {
		t.Fatal(err)
	}
	users := repository.New(db).User
	user := &model.User{Base: model.Base{ID: "account-1"}, Username: "actual-alice", PasswordHash: "database-secret-hash", Role: "user"}
	if err := users.Create(context.Background(), user); err != nil {
		t.Fatal(err)
	}
	queries := 0
	if err := db.Callback().Query().Before("gorm:query").Register("details_test_lookup", func(_ *gorm.DB) { queries++ }); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		userID      string
		trustedName string
		nilRepo     bool
		cancelled   bool
		username    string
		state       string
		queries     int
	}{
		{name: "fallback", userID: "account-1", username: "actual-alice", state: "known", queries: 1},
		{name: "trusted_name", userID: "account-1", trustedName: "already-loaded-alice", username: "already-loaded-alice", state: "known"},
		{name: "unknown_user", userID: "missing-account", state: "unavailable", queries: 1},
		{name: "nil_repository", userID: "account-1", nilRepo: true, state: "unavailable"},
		{name: "cancelled", userID: "account-1", cancelled: true, state: "unavailable"},
		{name: "anonymous", state: "anonymous"},
	} {
		t.Run(test.name, func(t *testing.T) {
			queries = 0
			core, logs := observer.New(zap.InfoLevel)
			r := gin.New()
			repo := users
			if test.nilRepo {
				repo = nil
			}
			r.Use(DetailedRequests(zap.New(core), repo))
			r.POST("/identity", func(c *gin.Context) {
				if test.userID != "" {
					c.Set(CtxUserID, test.userID)
				}
				if test.trustedName != "" {
					c.Set(CtxUserName, test.trustedName)
				}
				if _, err := io.ReadAll(c.Request.Body); err != nil {
					t.Fatal(err)
				}
				c.Status(http.StatusNoContent)
			})
			req := httptest.NewRequest(http.MethodPost, "/identity?UserId=claimed-other&username=query-pretender", strings.NewReader(`{"Username":"body-pretender","UserId":"claimed-other"}`))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Emby-UserName", "header-pretender")
			if test.cancelled {
				ctx, cancel := context.WithCancel(req.Context())
				cancel()
				req = req.WithContext(ctx)
			}
			r.ServeHTTP(httptest.NewRecorder(), req)
			fields := logs.All()[0].ContextMap()
			if fields["user_id"] != test.userID || fields["username"] != test.username || fields["username_state"] != test.state {
				t.Fatalf("trusted identity mismatch: %+v", fields)
			}
			if !test.cancelled && queries != test.queries {
				t.Fatalf("repository lookups = %d, want %d", queries, test.queries)
			}
			data, _ := json.Marshal(fields)
			if bytes.Contains(data, []byte("database-secret-hash")) {
				t.Fatal("repository user credentials exposed")
			}
		})
	}
}
