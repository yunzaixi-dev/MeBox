package middleware

import (
	"context"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"

	"github.com/truewhile/MeBox/internal/perftrace"
	"github.com/truewhile/MeBox/internal/repository"
)

const detailedRequestBodyLimit = 1 << 20

type detailedRequestKey struct{}

type detailedRequestState struct {
	id          string
	started     time.Time
	method      string
	path        string
	query       url.Values
	headers     http.Header
	client      embyCompatClient
	contentType string
	length      int64
	body        *detailedRequestBody
}

// DetailedRequests writes one private, sanitized event per real HTTP request.
// Mount it before Recovery and after PerformanceTrace, when tracing is enabled.
func DetailedRequests(log *zap.Logger, users *repository.UserRepository) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := c.Request.Context()
		if _, exists := ctx.Value(detailedRequestKey{}).(*detailedRequestState); exists {
			// HandleContext resets Gin's keys, but retains the request context.
			c.Next()
			return
		}
		state := &detailedRequestState{
			started:     time.Now(),
			method:      c.Request.Method,
			path:        sanitizeRequestURL(c.Request.URL.Path),
			query:       sanitizeRequestQuery(c.Request.URL.Query()),
			headers:     sanitizeRequestHeaders(c.Request.Header),
			client:      embyCompatClientInfo(c),
			contentType: c.GetHeader("Content-Type"),
			length:      c.Request.ContentLength,
		}
		if trace := perftrace.From(ctx); trace != nil {
			state.id, state.started = trace.ID, trace.StartedAt()
		} else {
			state.id = uuid.NewString()
		}
		c.Header("X-Request-ID", state.id)
		c.Request = c.Request.WithContext(context.WithValue(ctx, detailedRequestKey{}, state))
		if c.Request.Body != nil && c.Request.Body != http.NoBody {
			mediaType, _, err := mime.ParseMediaType(state.contentType)
			encoding := strings.TrimSpace(c.GetHeader("Content-Encoding"))
			supported := err == nil && (mediaType == "application/json" ||
				strings.HasSuffix(mediaType, "+json") || mediaType == "application/x-www-form-urlencoded" || mediaType == "text/plain") &&
				(encoding == "" || strings.EqualFold(encoding, "identity"))
			state.body = &detailedRequestBody{
				ReadCloser: c.Request.Body,
				supported:  supported,
				length:     state.length,
				truncated:  state.length > detailedRequestBodyLimit,
			}
			c.Request.Body = state.body
		}
		defer func() {
			var body any
			bodyState := "empty"
			var readBytes int64
			var readError bool
			if state.body != nil {
				body, bodyState, readBytes, readError = state.body.finish(state.contentType)
			}
			userID := c.GetString(CtxUserID)
			username, usernameState := "", "anonymous"
			if userID != "" {
				username = c.GetString(CtxUserName)
				if username == "" && users != nil {
					if user, err := users.FindByID(ctx, userID); err == nil && user != nil {
						username = user.Username
					}
				}
				usernameState = "unavailable"
				if username != "" {
					usernameState = "known"
				}
			}
			route := c.FullPath()
			if route == "" {
				route = "<unmatched>"
			}
			responseHeaders := sanitizeRequestHeaders(c.Writer.Header())
			log.Info("request_details",
				zap.String("request_id", state.id),
				zap.Time("started_at", state.started),
				zap.String("user_id", userID),
				zap.String("username", username),
				zap.String("username_state", usernameState),
				zap.Bool("anonymous", userID == ""),
				zap.String("method", state.method),
				zap.String("path", state.path),
				zap.String("normalized_path", sanitizeRequestURL(c.Request.URL.Path)),
				zap.String("route", route),
				zap.Any("query", state.query),
				zap.Any("headers", state.headers),
				zap.Any("body", body),
				zap.String("body_state", bodyState),
				zap.Int64("body_read_bytes", readBytes),
				zap.Bool("body_read_error", readError),
				zap.String("content_type", sanitizeRequestURL(state.contentType)),
				zap.Int64("content_length", state.length),
				zap.String("client", sanitizeRequestURL(state.client.Client)),
				zap.String("client_version", sanitizeRequestURL(state.client.Version)),
				zap.String("device", sanitizeRequestURL(state.client.Device)),
				zap.String("device_id", sanitizeRequestURL(state.client.DeviceID)),
				zap.String("claimed_user_id", sanitizeRequestURL(state.client.UserID)),
				zap.String("session_id", detailedRequestIdentity(state.query, body, "PlaySessionId", "SessionId")),
				zap.String("profile_id", detailedRequestIdentity(state.query, body, "PlayProfileId", "ProfileId", "DeviceProfileId", "play_profile_id")),
				zap.String("requested_media_source_id", detailedRequestIdentity(state.query, body, "MediaSourceId")),
				zap.String("ip", c.ClientIP()),
				zap.Int("status", c.Writer.Status()),
				zap.Int("response_bytes", max(0, c.Writer.Size())),
				zap.Any("response_headers", responseHeaders),
				zap.String("response_content_type", responseHeaders.Get("Content-Type")),
				zap.String("response_content_range", responseHeaders.Get("Content-Range")),
				zap.String("response_content_length", responseHeaders.Get("Content-Length")),
				zap.Duration("duration", time.Since(state.started)),
				zap.Bool("cancelled", ctx.Err() != nil),
			)
		}()
		c.Next()
	}
}

// Only string identities from sanitized input are promoted; claimed values are
// never used as authentication or represented as server-selected sources.
func detailedRequestIdentity(query url.Values, body any, names ...string) string {
	for _, name := range names {
		key, identity := "", ""
		for candidate, values := range query {
			if strings.EqualFold(candidate, name) && len(values) > 0 && (key == "" || candidate < key) {
				key, identity = candidate, values[0]
			}
		}
		if key != "" {
			return identity
		}
		if object, ok := body.(map[string]any); ok {
			for candidate, value := range object {
				text, isString := value.(string)
				if isString && strings.EqualFold(candidate, name) && (key == "" || candidate < key) {
					key, identity = candidate, text
				}
			}
		}
		if form, ok := body.(url.Values); ok {
			for candidate, values := range form {
				if strings.EqualFold(candidate, name) && len(values) > 0 && (key == "" || candidate < key) {
					key, identity = candidate, values[0]
				}
			}
		}
		if key != "" {
			return identity
		}
	}
	return ""
}

// This tap only observes reads made by handlers. Close goes directly to the
// original body, including when it must interrupt a blocked Read.
type detailedRequestBody struct {
	io.ReadCloser
	mu        sync.Mutex
	supported bool
	length    int64
	data      []byte
	readBytes int64
	read      bool
	eof       bool
	readError bool
	truncated bool
	finished  bool
}

func (b *detailedRequestBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.finished {
		return n, err
	}
	b.read = true
	b.readBytes += int64(n)
	b.eof = b.eof || err == io.EOF
	b.readError = b.readError || (err != nil && err != io.EOF)
	if b.readBytes > detailedRequestBodyLimit {
		b.truncated = true
		b.data = nil
	}
	if b.supported && !b.truncated && n > 0 {
		needed := len(b.data) + n
		if needed > cap(b.data) {
			capacity := min(detailedRequestBodyLimit, max(needed, max(512, 2*cap(b.data))))
			data := make([]byte, len(b.data), capacity)
			copy(data, b.data)
			b.data = data
		}
		b.data = append(b.data, p[:n]...)
	}
	return n, err
}

func (b *detailedRequestBody) finish(contentType string) (any, string, int64, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.finished = true
	defer func() { b.data = nil }()
	var body any
	state := "complete"
	switch {
	case !b.supported:
		state = "unsupported"
	case !b.read:
		state = "unread"
	case b.truncated:
		state = "truncated"
	case b.readError || (!b.eof && (b.length < 0 || b.readBytes != b.length)) || (b.length >= 0 && b.readBytes != b.length):
		state = "incomplete"
	default:
		body, state = sanitizeRequestBody(b.data, contentType)
	}
	return body, state, b.readBytes, b.readError
}
