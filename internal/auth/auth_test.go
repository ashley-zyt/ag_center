package auth

import (
	"bytes"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestSignVerifyRoundTrip(t *testing.T) {
	cfg := Config{APIKey: "test-key", APISecret: "test-secret"}

	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(b)
	}))

	body := []byte(`{"profile_name":"fb001","title":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/facebook/publish", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if err := SignRequest(cfg, req, "fixed-nonce"); err != nil {
		t.Fatalf("SignRequest: %v", err)
	}

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rr.Code, rr.Body.String())
	}
	if rr.Body.String() != string(body) {
		t.Fatalf("body not preserved, got %q", rr.Body.String())
	}
}

func TestRejectMissingKey(t *testing.T) {
	cfg := Config{APIKey: "test-key", APISecret: "test-secret"}
	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/twitter/publish", bytes.NewReader([]byte(`{}`)))
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestRejectTamperedBody(t *testing.T) {
	cfg := Config{APIKey: "test-key", APISecret: "test-secret"}
	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := []byte(`{"profile_name":"fb001"}`)
	req := httptest.NewRequest(http.MethodPost, "/facebook/publish", bytes.NewReader(body))
	if err := SignRequest(cfg, req, "nonce-1"); err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	// Tamper with the body after signing.
	req.Body = io.NopCloser(bytes.NewReader([]byte(`{"profile_name":"evil"}`)))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for tampered body, got %d", rr.Code)
	}
}

func TestRejectReplay(t *testing.T) {
	cfg := Config{APIKey: "test-key", APISecret: "test-secret"}
	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := []byte(`{"profile_name":"fb001"}`)
	makeReq := func() *http.Request {
		req := httptest.NewRequest(http.MethodPost, "/facebook/publish", bytes.NewReader(body))
		_ = SignRequest(cfg, req, "replay-nonce")
		return req
	}

	rr1 := httptest.NewRecorder()
	handler.ServeHTTP(rr1, makeReq())
	if rr1.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d: %s", rr1.Code, rr1.Body.String())
	}

	rr2 := httptest.NewRecorder()
	handler.ServeHTTP(rr2, makeReq())
	if rr2.Code != http.StatusUnauthorized {
		t.Fatalf("replayed request should be rejected, got %d", rr2.Code)
	}
}

func TestRejectExpiredTimestamp(t *testing.T) {
	cfg := Config{APIKey: "test-key", APISecret: "test-secret"}
	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/facebook/publish", bytes.NewReader(body))
	if err := SignRequest(cfg, req, "nonce-exp"); err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	// Rewind the timestamp to 20 minutes ago (outside the 15-minute skew).
	old := time.Now().Add(-20 * time.Minute).Unix()
	oldStr := strconv.FormatInt(old, 10)
	req.Header.Set(HeaderTimestamp, oldStr)
	// Recompute the signature for the old timestamp.
	req.Header.Set(HeaderSignature, hex.EncodeToString(sign(cfg.APISecret, req.Method, req.URL.Path, oldStr, "nonce-exp", body)))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expired timestamp should be rejected, got %d", rr.Code)
	}
}

func TestHealthExempt(t *testing.T) {
	cfg := Config{APIKey: "test-key", APISecret: "test-secret"}
	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("health should be exempt, got %d", rr.Code)
	}
}

func TestConfigurableSkewAllowsOldTimestamp(t *testing.T) {
	cfg := Config{APIKey: "test-key", APISecret: "test-secret", MaxSkew: 30 * time.Minute}
	handler := Middleware(cfg, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	body := []byte(`{}`)
	req := httptest.NewRequest(http.MethodPost, "/facebook/publish", bytes.NewReader(body))
	old := time.Now().Add(-20 * time.Minute).Unix()
	oldStr := strconv.FormatInt(old, 10)
	if err := SignRequest(cfg, req, "nonce-old"); err != nil {
		t.Fatalf("SignRequest: %v", err)
	}
	req.Header.Set(HeaderTimestamp, oldStr)
	req.Header.Set(HeaderSignature, hex.EncodeToString(sign(cfg.APISecret, req.Method, req.URL.Path, oldStr, "nonce-old", body)))

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("timestamp within configured skew should pass, got %d: %s", rr.Code, rr.Body.String())
	}
}
