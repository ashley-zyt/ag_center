// Package auth provides HTTP API authentication for the publish service.
//
// It combines two layers of protection:
//  1. API key: the caller must send a shared secret in the X-API-Key header,
//     compared in constant time to avoid timing side channels.
//  2. HMAC-SHA256 request signature: the caller signs the method, path,
//     timestamp, nonce and body hash with a shared secret, which protects the
//     request against tampering and replay.
//
// Secrets are read from environment variables:
//   - API_KEY    (required) value for the X-API-Key header
//   - API_SECRET (optional) HMAC signing key; defaults to API_KEY if unset.
//     Using a distinct API_SECRET is recommended so that a leaked API key
//     (often visible in access logs) does not also let an attacker forge
//     signatures.
//
// The /health path is exempt from authentication so load balancers can probe
// the service.
package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Header names used by the protocol.
const (
	HeaderAPIKey    = "X-API-Key"
	HeaderTimestamp = "X-Timestamp"
	HeaderNonce     = "X-Nonce"
	HeaderSignature = "X-Signature"

	maxBodyBytes = 1 << 20 // 1MB, matches the handler-side body limit

	// defaultMaxSkew is the default allowed clock-skew between the client and
	// this server, in seconds tolerance both directions.
	defaultMaxSkew = 15 * time.Minute
)

// Config holds the secrets used for authentication.
type Config struct {
	APIKey    string
	APISecret string
	// MaxSkew is the allowed clock-skew between the client and this server.
	// Zero means "use the default" (defaultMaxSkew).
	MaxSkew time.Duration
}

// skew returns the effective clock-skew tolerance.
func (c Config) skew() time.Duration {
	if c.MaxSkew <= 0 {
		return defaultMaxSkew
	}
	return c.MaxSkew
}

// LoadConfig reads API_KEY, API_SECRET and AUTH_MAX_SKEW_SECONDS from the
// environment. API_KEY is required; API_SECRET falls back to API_KEY when
// unset. AUTH_MAX_SKEW_SECONDS (integer seconds) overrides the default 15
// minute clock-skew tolerance — mainly useful when the server clock is
// intentionally not synced; a large value weakens replay protection.
func LoadConfig() (Config, error) {
	key := os.Getenv("API_KEY")
	if key == "" {
		return Config{}, errors.New("API_KEY environment variable is required (set it to a long random secret)")
	}
	secret := os.Getenv("API_SECRET")
	if secret == "" {
		secret = key
	}
	skew := defaultMaxSkew
	if v := os.Getenv("AUTH_MAX_SKEW_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			skew = time.Duration(n) * time.Second
		}
	}
	return Config{APIKey: key, APISecret: secret, MaxSkew: skew}, nil
}

// Middleware wraps a handler with API-key + HMAC signature authentication.
func Middleware(cfg Config, next http.Handler) http.Handler {
	nonces := newNonceStore(cfg.skew())
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}
		if err := authenticate(cfg, nonces, r); err != nil {
			writeUnauthorized(w, err.Error())
			return
		}
		next.ServeHTTP(w, r)
	})
}

// authenticate validates the API key and HMAC signature of a request.
// On success it restores r.Body so the downstream handler can read it.
func authenticate(cfg Config, nonces *nonceStore, r *http.Request) error {
	// 1. API key, compared in constant time.
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(HeaderAPIKey)), []byte(cfg.APIKey)) != 1 {
		return errors.New("invalid or missing api key")
	}

	// 2. Read the body (and restore it) so it can be included in the signature.
	var body []byte
	if r.Body != nil {
		raw, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
		if err != nil {
			return errors.New("read body failed: " + err.Error())
		}
		if int64(len(raw)) > maxBodyBytes {
			return errors.New("body too large")
		}
		body = raw
		r.Body = io.NopCloser(bytes.NewReader(raw))
	}

	// 3. Timestamp, with clock-skew tolerance to defeat replay.
	tsStr := r.Header.Get(HeaderTimestamp)
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return errors.New("missing or invalid timestamp")
	}
	now := time.Now()
	reqTime := time.Unix(ts, 0)
	if d := now.Sub(reqTime); d > cfg.skew() || d < -cfg.skew() {
		return errors.New("timestamp outside allowed skew")
	}

	// 4. Nonce, for replay protection.
	nonce := r.Header.Get(HeaderNonce)
	if nonce == "" {
		return errors.New("missing nonce")
	}
	if !nonces.checkAndStore(nonce, now) {
		return errors.New("duplicate nonce (possible replay)")
	}

	// 5. Signature verification.
	expected := hex.EncodeToString(sign(cfg.APISecret, r.Method, r.URL.Path, tsStr, nonce, body))
	if subtle.ConstantTimeCompare([]byte(r.Header.Get(HeaderSignature)), []byte(expected)) != 1 {
		return errors.New("signature mismatch")
	}

	return nil
}

// sign computes the HMAC-SHA256 signature over the canonical request string:
//
//	method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + bodyHash
//
// where bodyHash is the lowercase hex SHA-256 of the raw request body bytes.
func sign(secret, method, path, timestamp, nonce string, body []byte) []byte {
	h := sha256.Sum256(body)
	canonical := method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + hex.EncodeToString(h[:])
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(canonical))
	return mac.Sum(nil)
}

// nonceStore tracks recently seen nonces to reject replays.
type nonceStore struct {
	mu  sync.Mutex
	m   map[string]time.Time
	ttl time.Duration
}

func newNonceStore(ttl time.Duration) *nonceStore {
	return &nonceStore{m: make(map[string]time.Time), ttl: ttl}
}

func (s *nonceStore) checkAndStore(nonce string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if t, ok := s.m[nonce]; ok && now.Sub(t) < s.ttl {
		return false
	}
	s.m[nonce] = now
	if len(s.m) > 10000 {
		for k, v := range s.m {
			if now.Sub(v) > s.ttl {
				delete(s.m, k)
			}
		}
	}
	return true
}

func writeUnauthorized(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(map[string]string{"type": "error", "error_info": msg})
}

// GenerateNonce returns a cryptographically random nonce.
func GenerateNonce() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SignRequest reads req.Body, computes the HMAC signature, sets the four auth
// headers, and restores req.Body so the request can still be sent.
func SignRequest(cfg Config, req *http.Request, nonce string) error {
	var body []byte
	if req.Body != nil {
		raw, err := io.ReadAll(req.Body)
		if err != nil {
			return err
		}
		body = raw
		req.Body = io.NopCloser(bytes.NewReader(raw))
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	if nonce == "" {
		nonce = GenerateNonce()
	}
	sum := sign(cfg.APISecret, req.Method, req.URL.Path, timestamp, nonce, body)
	req.Header.Set(HeaderAPIKey, cfg.APIKey)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, hex.EncodeToString(sum))
	return nil
}

// NewClient returns an *http.Client that automatically signs every request.
func NewClient(cfg Config) *http.Client {
	return &http.Client{Transport: &signingTransport{cfg: cfg}}
}

type signingTransport struct {
	cfg  Config
	base http.RoundTripper
}

func (t *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := SignRequest(t.cfg, req, ""); err != nil {
		return nil, err
	}
	if t.base == nil {
		t.base = http.DefaultTransport
	}
	return t.base.RoundTrip(req)
}
