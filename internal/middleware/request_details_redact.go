package middleware

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

const requestDetailRedacted = "[REDACTED]"

// Match URLs inside header/text values, not just values consisting of a URL.
// Relative URLs with a query also occur in redirect and playback fields.
var requestDetailURLs = regexp.MustCompile(`(?i)(?:[a-z][a-z0-9+.-]*://|//)[^\s<>"']+|[a-z0-9._~!$&()*+,;=:@%/-]*\?[^\s<>"']+`)

func requestDetailCredentialKey(key string) bool {
	key = strings.ToLower(strings.Map(func(r rune) rune {
		switch r {
		case '-', '_', '.', ' ':
			return -1
		default:
			return r
		}
	}, key))
	switch key {
	case "pw", "pwd", "pass", "pin", "pins", "currentpin", "newpin", "oldpin", "profilepin", "playprofilepin", "parentalpin", "devicepin", "userpin",
		"auth", "oauthcode", "authcode", "authorizationcode", "jwt", "csrf", "xsrf", "credential", "credentials", "sig", "keypairid", "googleaccessid":
		return true
	}
	for _, suffix := range [...]string{
		"password", "passwd", "passwordhash", "passwordmd5", "passwordsha1", "passwordsha256", "passwordsha512",
		"pinhash", "pinmd5", "pinsha1", "pinsha256", "token", "tokens", "apikey", "apikeys", "authkey", "sessionkey", "accesskey", "accesskeyid", "secret", "secrets", "secretkey", "privatekey", "privatekeys", "signingkey", "signingkeys",
		"authorization", "cookie", "cookies", "signature", "credential", "credentials",
	} {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

func sanitizeRequestHeaders(headers http.Header) http.Header {
	clean, _ := sanitizeRequestValues(url.Values(headers), 0)
	return http.Header(clean)
}

func sanitizeRequestQuery(query url.Values) url.Values {
	clean, _ := sanitizeRequestValues(query, 0)
	return clean
}

func sanitizeRequestValues(values url.Values, depth int) (url.Values, bool) {
	clean := make(url.Values, len(values))
	changed := false
	credentialValue := false
	for key, entries := range values {
		if strings.EqualFold(key, "key") || strings.EqualFold(key, "name") {
			for _, entry := range entries {
				credentialValue = credentialValue || requestDetailCredentialKey(entry)
			}
		}
	}
	for key, entries := range values {
		clean[key] = make([]string, len(entries))
		credential := requestDetailCredentialKey(key) || strings.EqualFold(key, "policy") || strings.EqualFold(key, "code") ||
			(depth > 0 && strings.EqualFold(key, "key")) || (credentialValue && strings.EqualFold(key, "value"))
		for i, entry := range entries {
			if credential {
				clean[key][i] = requestDetailRedacted
			} else {
				clean[key][i] = sanitizeRequestText(entry, depth)
			}
			changed = changed || clean[key][i] != entry
		}
	}
	return clean, changed
}

func sanitizeRequestBody(body []byte, contentType string) (any, string) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, "unsupported"
	}
	switch {
	case mediaType == "application/json" || strings.HasSuffix(mediaType, "+json"):
		value, valid := decodeRequestDetailJSON(body)
		if !valid {
			return nil, "invalid_json"
		}
		return sanitizeRequestJSON(value, 0), "complete"
	case mediaType == "text/plain":
		value, valid := decodeRequestDetailJSON(body)
		switch value.(type) {
		case map[string]any, []any:
			if valid {
				return sanitizeRequestJSON(value, 0), "complete"
			}
		}
		return nil, "unsupported"
	case mediaType == "application/x-www-form-urlencoded":
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return nil, "unsupported"
		}
		return sanitizeRequestQuery(values), "complete"
	default:
		return nil, "unsupported"
	}
}

func decodeRequestDetailJSON(body []byte) (any, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if decoder.Decode(&value) != nil {
		return nil, false
	}
	// A second document or malformed trailing data must never reach the log.
	var extra any
	return value, decoder.Decode(&extra) == io.EOF
}

func sanitizeRequestJSON(value any, depth int) any {
	switch value := value.(type) {
	case map[string]any:
		credentialValue := false
		for key, entry := range value {
			if strings.EqualFold(key, "key") || strings.EqualFold(key, "name") {
				if name, ok := entry.(string); ok {
					credentialValue = credentialValue || requestDetailCredentialKey(name)
				}
			}
		}
		for key, entry := range value {
			if requestDetailCredentialKey(key) || (credentialValue && strings.EqualFold(key, "value")) {
				value[key] = requestDetailRedacted
			} else {
				value[key] = sanitizeRequestJSON(entry, depth)
			}
		}
		return value
	case []any:
		for i, entry := range value {
			value[i] = sanitizeRequestJSON(entry, depth)
		}
		return value
	case string:
		return sanitizeRequestText(value, depth)
	default:
		return value
	}
}

func sanitizeRequestURL(value string) string {
	return sanitizeRequestText(value, 0)
}

func sanitizeRequestText(value string, depth int) string {
	// Nested redirects may be repeatedly percent-encoded. Bound their parsing,
	// and fail closed rather than returning unsanitized deep credentials.
	if depth >= 16 {
		return requestDetailRedacted
	}
	trimmed := strings.TrimSpace(value)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		parsed, valid := decodeRequestDetailJSON([]byte(trimmed))
		if !valid {
			return requestDetailRedacted
		}
		encoded, err := json.Marshal(sanitizeRequestJSON(parsed, depth+1))
		if err != nil {
			return requestDetailRedacted
		}
		return string(encoded)
	}
	if !strings.Contains(value, "://") && !strings.Contains(value, "?") && strings.Contains(value, "%") {
		if decoded, err := url.QueryUnescape(value); err == nil && decoded != value {
			if clean := sanitizeRequestText(decoded, depth+1); clean != decoded {
				return url.QueryEscape(clean)
			}
		}
	}
	return requestDetailURLs.ReplaceAllStringFunc(value, func(raw string) string {
		parsed, err := url.Parse(raw)
		if err != nil {
			return requestDetailRedacted
		}
		changed := false
		if parsed.User != nil {
			parsed.User = url.User(requestDetailRedacted)
			changed = true
		}
		if parsed.RawQuery != "" {
			query, err := url.ParseQuery(parsed.RawQuery)
			if err != nil {
				parsed.RawQuery = url.QueryEscape(requestDetailRedacted)
				changed = true
			} else {
				clean, queryChanged := sanitizeRequestValues(query, depth+1)
				if queryChanged {
					parsed.RawQuery = clean.Encode()
					changed = true
				}
			}
		}
		if parsed.Fragment != "" {
			clean := sanitizeRequestText(parsed.Fragment, depth+1)
			if strings.Contains(parsed.Fragment, "=") {
				fragment, err := url.ParseQuery(parsed.Fragment)
				if err != nil {
					clean = requestDetailRedacted
				} else if sanitized, fragmentChanged := sanitizeRequestValues(fragment, depth+1); fragmentChanged {
					clean = sanitized.Encode()
				}
			}
			if clean != parsed.Fragment {
				parsed.Fragment, parsed.RawFragment = clean, ""
				changed = true
			}
		}
		if changed {
			return parsed.String()
		}
		return raw
	})
}
