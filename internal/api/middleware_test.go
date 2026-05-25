package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newDiscardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestRecoveryMW_RecoversAndWrites500(t *testing.T) {
	h := recoveryMW(newDiscardLogger())(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/whatever", nil)
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "internal_panic") {
		t.Fatalf("body should mention internal_panic: %s", rec.Body.String())
	}
}

func TestLoggingMW_NoTokenLeak(t *testing.T) {
	var sink bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&sink, nil))
	h := loggingMW(logger)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/x", nil)
	req.Header.Set("Authorization", "Bearer SUPER-SECRET-TOKEN")
	h.ServeHTTP(rec, req)
	if strings.Contains(sink.String(), "SUPER-SECRET-TOKEN") {
		t.Fatalf("bearer token leaked into log: %s", sink.String())
	}
	if !strings.Contains(sink.String(), "status=418") {
		t.Fatalf("expected status to be logged: %s", sink.String())
	}
}

func TestOriginMW_AllowsLocalhost(t *testing.T) {
	mw := originMW(OriginConfig{ServerBoundLocal: true})
	cases := []string{
		"http://127.0.0.1:7821",
		"http://localhost:5173",
		"https://127.0.0.1:8443",
	}
	for _, origin := range cases {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
		req.Header.Set("Origin", origin)
		mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("origin %q rejected (status=%d): %s", origin, rec.Code, rec.Body.String())
		}
	}
}

func TestOriginMW_RejectsHomograph(t *testing.T) {
	mw := originMW(OriginConfig{ServerBoundLocal: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Origin", "http://127.0.0.1.evil.com")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("homograph origin must be rejected, got %d", rec.Code)
	}
}

func TestOriginMW_MissingOriginPublicBind(t *testing.T) {
	mw := originMW(OriginConfig{ServerBoundLocal: false})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing origin on public bind must be rejected, got %d", rec.Code)
	}
}

func TestOriginMW_MissingOriginLocalhost(t *testing.T) {
	mw := originMW(OriginConfig{ServerBoundLocal: true})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("missing origin on localhost must be allowed, got %d", rec.Code)
	}
}

func TestOriginMW_NullOriginGated(t *testing.T) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Origin", "null")
	originMW(OriginConfig{ServerBoundLocal: true, AllowNullOrigin: false})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("null origin default-deny failed, got %d", rec.Code)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Origin", "null")
	originMW(OriginConfig{ServerBoundLocal: true, AllowNullOrigin: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("null origin opt-in failed, got %d", rec.Code)
	}
}

func TestOriginMW_HealthAndUIBypassed(t *testing.T) {
	mw := originMW(OriginConfig{ServerBoundLocal: false})
	for _, path := range []string{"/health", "/ui", "/ui/index.html"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		// No Origin and public bind would normally reject — but path is exempt.
		mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s must bypass origin check, got %d", path, rec.Code)
		}
	}
}

func TestOriginMW_CustomWhitelist(t *testing.T) {
	mw := originMW(OriginConfig{Allowed: []string{"http://my-ui.internal:3000"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Origin", "http://my-ui.internal:3000")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("custom whitelist failed, got %d", rec.Code)
	}
}

func TestBearerMW_HealthAndUIBypassed(t *testing.T) {
	mw := bearerMW(BearerConfig{Enabled: true, Token: "abc"})
	for _, path := range []string{"/health", "/ui", "/ui/anything"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s must bypass bearer, got %d", path, rec.Code)
		}
	}
}

func TestBearerMW_AuthRequiredAndInvalid(t *testing.T) {
	mw := bearerMW(BearerConfig{Enabled: true, Token: "secret"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token: want 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "auth_required") {
		t.Fatalf("body: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer wrong")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: want 401, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "invalid_token") {
		t.Fatalf("body: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	req.Header.Set("Authorization", "Bearer secret")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusTeapot) })).ServeHTTP(rec, req)
	if rec.Code != http.StatusTeapot {
		t.Fatalf("correct token rejected: %d", rec.Code)
	}
}

func TestBearerMW_DisabledIsPassthrough(t *testing.T) {
	mw := bearerMW(BearerConfig{Enabled: false})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/sessions", nil)
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("disabled bearer should not block, got %d", rec.Code)
	}
}

func TestMaxBytesMW_LimitsReadInHandler(t *testing.T) {
	mw := maxBytesMW(8)
	called := false
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/foo",
		strings.NewReader(`{"x":"abcdefghijk"}`))
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, err := io.ReadAll(r.Body)
		if !isMaxBytesError(err) {
			t.Fatalf("expected MaxBytesError, got %v", err)
		}
		writeRequestTooLarge(w, 8)
	})).ServeHTTP(rec, req)
	if !called {
		t.Fatal("handler not invoked")
	}
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: %d", rec.Code)
	}
}

func TestWriteRequestTooLarge_Shape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeRequestTooLarge(rec, 1024)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: %d", rec.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body unmarshal: %v", err)
	}
	if body["code"] != "request_too_large" || body["limit"].(float64) != 1024 {
		t.Fatalf("body: %+v", body)
	}
}
