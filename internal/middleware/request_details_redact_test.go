package middleware

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestSanitizeRequestBodyPreservesProfileAndTypes(t *testing.T) {
	body := []byte(`{
		"UserId":"user-1", "DeviceId":"device-1", "SessionId":"session-1",
		"DeviceProfile": {
			"MinSegments":"0004",
			"Conditions":[{"MinSegments":9007199254740993,"ExactDecimal":1.234567890123456789,"Enabled":true,"Codec":"h264","TokenType":"Bearer","Policy":{"Enabled":true}}],
			"Source":"https://url-user:url-password@media.test/play?api_key=url-key&MinSegments=4"
		},
		"Options":[{"Access_Token":"access-value","Refresh-Token":"refresh-value"}],
		"Nested":{"Pw":"pw-value","PIN":"pin-value","PasswordMd5":"hash-value","Signing_Key":"signing-value","private-key":"private-value","OAuth_Code":"oauth-value"}
	}`)
	value, state := sanitizeRequestBody(body, "application/json; charset=utf-8")
	if state != "complete" {
		t.Fatalf("state = %q", state)
	}
	object := value.(map[string]any)
	if object["UserId"] != "user-1" || object["DeviceId"] != "device-1" || object["SessionId"] != "session-1" {
		t.Fatalf("business IDs changed: %#v", object)
	}
	profile := object["DeviceProfile"].(map[string]any)
	if profile["MinSegments"] != "0004" {
		t.Fatalf("string MinSegments changed: %#v", profile["MinSegments"])
	}
	condition := profile["Conditions"].([]any)[0].(map[string]any)
	if condition["MinSegments"] != json.Number("9007199254740993") || condition["ExactDecimal"] != json.Number("1.234567890123456789") {
		t.Fatalf("numeric precision/type changed: %#v", condition)
	}
	if condition["Enabled"] != true || condition["Codec"] != "h264" || condition["TokenType"] != "Bearer" || condition["Policy"].(map[string]any)["Enabled"] != true {
		t.Fatalf("ordinary profile fields changed: %#v", condition)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"url-user", "url-password", "url-key", "access-value", "refresh-value", "pw-value", "pin-value", "hash-value", "signing-value", "private-value", "oauth-value"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("credential %q leaked in %s", secret, encoded)
		}
	}
}

func TestSanitizeRequestBodyRejectsMalformedAndTrailingJSON(t *testing.T) {
	for _, body := range []string{
		`{"Password":"do-not-log"`,
		`{"Password":"do-not-log"} trailing`,
		`{"Password":"do-not-log"} {"UserId":"user-1"}`,
	} {
		value, state := sanitizeRequestBody([]byte(body), "application/problem+json")
		if value != nil || state != "invalid_json" {
			t.Fatalf("malformed JSON returned data: %#v, %q", value, state)
		}
	}
	for _, contentType := range []string{"text/plain", "multipart/form-data; boundary=test", "application/octet-stream"} {
		value, state := sanitizeRequestBody([]byte("Password=do-not-log"), contentType)
		if value != nil || state != "unsupported" {
			t.Fatalf("unsupported body returned data: %#v, %q", value, state)
		}
	}
}

func TestSanitizeRequestHeadersQueryAndForm(t *testing.T) {
	headers := http.Header{
		"Authorization":        {"Bearer auth-value"},
		"Cookie":               {"session=cookie-value"},
		"X-Emby-Authorization": {`MediaBrowser Client="client", Token="emby-value"`},
		"X-Api-Key":            {"header-key"},
		"Range":                {"bytes=1024-2047"},
		"X-Device-Id":          {"device-1"},
		"Forwarded":            {`for=127.0.0.1;host="https://forward-user:forward-password@media.test/?signature=forward-signature";proto=https`},
		"Referer":              {"https://outer.test/?return=https%3A%2F%2Finner-user%3Ainner-password%40inner.test%2F%3Fsignature%3Dinner-signature%26MinSegments%3D4"},
	}
	clean := sanitizeRequestHeaders(headers)
	if clean.Get("Range") != "bytes=1024-2047" || clean.Get("X-Device-Id") != "device-1" || !strings.Contains(clean.Get("Referer"), "MinSegments") {
		t.Fatalf("useful headers lost: %#v", clean)
	}
	encoded, err := json.Marshal(clean)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"auth-value", "cookie-value", "emby-value", "header-key", "forward-user", "forward-password", "forward-signature", "inner-user", "inner-password", "inner-signature"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("header credential %q leaked: %s", secret, encoded)
		}
	}
	if headers.Get("Authorization") != "Bearer auth-value" {
		t.Fatal("sanitizer mutated the live request headers")
	}
	query := url.Values{
		"UserId": {"user-1"}, "SessionId": {"session-1"}, "MinSegments": {"004", "5"},
		"access_token": {"query-token"}, "X-Emby-Authorization": {"Token=query-auth"},
		"ReturnUrl": {"https://media.test/?X-Amz-Credential=query-credential&X-Amz-Signature=query-signature"},
	}
	cleanQuery := sanitizeRequestQuery(query)
	if cleanQuery.Get("UserId") != "user-1" || cleanQuery.Get("SessionId") != "session-1" || strings.Join(cleanQuery["MinSegments"], ",") != "004,5" {
		t.Fatalf("ordinary query values changed: %#v", cleanQuery)
	}
	for _, secret := range []string{"query-token", "query-auth", "query-credential", "query-signature"} {
		if strings.Contains(cleanQuery.Encode(), secret) {
			t.Fatalf("query credential %q leaked: %#v", secret, cleanQuery)
		}
	}
	form, state := sanitizeRequestBody([]byte("Pw=form-password&refresh_token=form-token&MinSegments=004&UserId=user-1"), "application/x-www-form-urlencoded")
	if state != "complete" {
		t.Fatalf("form state = %q", state)
	}
	fields := form.(url.Values)
	if fields.Get("MinSegments") != "004" || fields.Get("UserId") != "user-1" || strings.Contains(fields.Encode(), "form-password") || strings.Contains(fields.Encode(), "form-token") {
		t.Fatalf("form sanitization = %#v", fields)
	}
	if query.Get("access_token") != "query-token" {
		t.Fatal("sanitizer mutated the live query")
	}
}

func TestSanitizeRequestURLEmbeddedAndSignedCredentials(t *testing.T) {
	for _, raw := range []string{
		"https://media.test/play?sig=hidden-credential&MinSegments=4",
		"/play?api-key=hidden-credential&MinSegments=4",
		"open https://media.test/play?X-Goog-Signature=hidden-credential&MinSegments=4 now",
		"https://media.test/#access_token=hidden-credential&MinSegments=4",
		"https%3A%2F%2Fmedia.test%2Fplay%3Fapi_key%3Dhidden-credential%26MinSegments%3D4",
		"https://media.test/?signature=hidden-credential;invalid=query",
	} {
		clean := sanitizeRequestURL(raw)
		decoded, _ := url.QueryUnescape(clean)
		if strings.Contains(clean, "hidden-credential") || strings.Contains(decoded, "hidden-credential") {
			t.Fatalf("signed credential leaked: %q", clean)
		}
		if strings.Contains(raw, "MinSegments") && !strings.Contains(clean, "MinSegments") {
			t.Fatalf("ordinary URL field lost: %q", clean)
		}
	}
	const safe = "https://media.test/play?UserId=user-1&MinSegments=004&codec=h264"
	if got := sanitizeRequestURL(safe); got != safe {
		t.Fatalf("safe URL changed: %q", got)
	}
}

func TestSanitizeRequestSettingAndNamedHeaderValues(t *testing.T) {
	for _, field := range []string{"key", "Name"} {
		for _, name := range []string{"telegram_bot_token", "api_keys", "scoped_tokens", "profile_PIN_hash", "Authorization"} {
			body, err := json.Marshal(map[string]any{field: name, "Value": "setting-secret"})
			if err != nil {
				t.Fatal(err)
			}
			value, state := sanitizeRequestBody(body, "application/json")
			object := value.(map[string]any)
			if state != "complete" || object[field] != name || object["Value"] == "setting-secret" {
				t.Fatalf("named credential was not sanitized: %#v, %q", value, state)
			}
			form := url.Values{field: {name}, "Value": {"setting-secret"}}
			value, state = sanitizeRequestBody([]byte(form.Encode()), "application/x-www-form-urlencoded")
			fields := value.(url.Values)
			if state != "complete" || fields.Get(field) != name || fields.Get("Value") == "setting-secret" {
				t.Fatalf("named form credential was not sanitized: %#v, %q", value, state)
			}
		}
	}
	value, state := sanitizeRequestBody([]byte(`{"key":"reader_enabled","value":"true","Code":"AV123","Codec":"h264","Api_Keys":["plural-secret"],"PIN_Hash":"pin-secret"}`), "application/json")
	object := value.(map[string]any)
	if state != "complete" || object["key"] != "reader_enabled" || object["value"] != "true" || object["Code"] != "AV123" || object["Codec"] != "h264" {
		t.Fatalf("ordinary setting/business values changed: %#v", value)
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "plural-secret") || strings.Contains(string(encoded), "pin-secret") {
		t.Fatalf("plural credential/PIN hash leaked: %s", encoded)
	}
	form, state := sanitizeRequestBody([]byte("key=reader_enabled&value=true"), "application/x-www-form-urlencoded")
	if state != "complete" || form.(url.Values).Get("key") != "reader_enabled" || form.(url.Values).Get("value") != "true" {
		t.Fatalf("ordinary form setting changed: %#v", form)
	}
}

func TestSanitizeRequestEmbeddedJSONStrings(t *testing.T) {
	for _, inner := range []string{
		`{"Authorization":"Bearer embedded-auth","Cookie":"sid=embedded-cookie","MinSegments":9007199254740993,"Codec":"h264"}`,
		`[{"name":"Authorization","value":"embedded-auth"},{"name":"Cookie","value":"embedded-cookie"},{"MinSegments":9007199254740993,"Codec":"h264"}]`,
	} {
		body, err := json.Marshal(map[string]any{"login_header": inner})
		if err != nil {
			t.Fatal(err)
		}
		value, state := sanitizeRequestBody(body, "application/json")
		clean, ok := value.(map[string]any)["login_header"].(string)
		if state != "complete" || !ok || strings.Contains(clean, "embedded-auth") || strings.Contains(clean, "embedded-cookie") {
			t.Fatalf("embedded JSON credentials leaked/string type lost: %#v", value)
		}
		if !strings.Contains(clean, "9007199254740993") || !strings.Contains(clean, "h264") || !json.Valid([]byte(clean)) {
			t.Fatalf("embedded business fields/precision changed: %q", clean)
		}
		form := url.Values{"login_header": {inner}}
		value, state = sanitizeRequestBody([]byte(form.Encode()), "application/x-www-form-urlencoded")
		clean = value.(url.Values).Get("login_header")
		if state != "complete" || strings.Contains(clean, "embedded-auth") || strings.Contains(clean, "embedded-cookie") || !json.Valid([]byte(clean)) {
			t.Fatalf("embedded form JSON leaked: %q", clean)
		}
	}
	for _, inner := range []string{`{"Password":"malformed-secret"`, `[{"Cookie":"malformed-secret"}] trailing`} {
		body, err := json.Marshal(map[string]any{"login_header": inner})
		if err != nil {
			t.Fatal(err)
		}
		value, state := sanitizeRequestBody(body, "application/json")
		clean, ok := value.(map[string]any)["login_header"].(string)
		if state != "complete" || !ok || strings.Contains(clean, "malformed-secret") {
			t.Fatalf("malformed embedded JSON leaked: %#v", value)
		}
	}
}
