package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// session holds an expiring server-side credential and independent CSRF token.
type session struct {
	csrf    string
	expires time.Time
}

// rateBucket is a bounded, fixed-window request counter.
type rateBucket struct {
	start time.Time
	count int
}

// Auth manages a password digest, bounded sessions and login rate limits.
type Auth struct {
	mu       sync.Mutex
	password [32]byte
	sessions map[string]session
	attempts map[string]rateBucket
	global   rateBucket
}

// newAuth hashes the supplied administrator password and creates empty session state.
func newAuth(password string) *Auth {
	return &Auth{password: sha256.Sum256([]byte(password)), sessions: map[string]session{}, attempts: map[string]rateBucket{}}
}

// randomToken returns a cryptographically random 256-bit hexadecimal identifier.
func randomToken() (string, error) {
	buffer := make([]byte, 32)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

// allowBucket consumes one token in a fixed window and reports whether it is allowed.
func allowBucket(bucket *rateBucket, now time.Time, limit int, window time.Duration) bool {
	if now.Sub(bucket.start) >= window {
		bucket.start = now
		bucket.count = 0
	}
	if bucket.count >= limit {
		return false
	}
	bucket.count++
	return true
}

// allowLogin applies per-peer and global limits, ignoring untrusted forwarding headers.
func (auth *Auth) allowLogin(remote string) bool {
	auth.mu.Lock()
	defer auth.mu.Unlock()
	now := time.Now()
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	for key, bucket := range auth.attempts {
		if now.Sub(bucket.start) > time.Minute {
			delete(auth.attempts, key)
		}
	}
	if !allowBucket(&auth.global, now, 60, time.Minute) {
		return false
	}
	bucket, exists := auth.attempts[host]
	if !exists && len(auth.attempts) >= 4096 {
		return false
	}
	allowed := allowBucket(&bucket, now, 10, time.Minute)
	auth.attempts[host] = bucket
	return allowed
}

// current retrieves an unexpired cookie session, or an empty unauthenticated result.
func (auth *Auth) current(request *http.Request) (session, bool) {
	cookie, err := request.Cookie("admin_session")
	if err != nil {
		return session{}, false
	}
	auth.mu.Lock()
	defer auth.mu.Unlock()
	value, ok := auth.sessions[cookie.Value]
	if !ok || time.Now().After(value.expires) {
		delete(auth.sessions, cookie.Value)
		return session{}, false
	}
	return value, true
}

// login verifies a constant-length hash in constant time and rotates the caller's session.
func (auth *Auth) login(writer http.ResponseWriter, request *http.Request, password string) (string, error) {
	digest := sha256.Sum256([]byte(password))
	if subtle.ConstantTimeCompare(digest[:], auth.password[:]) != 1 {
		return "", errors.New("invalid password")
	}
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	csrf, err := randomToken()
	if err != nil {
		return "", err
	}
	auth.mu.Lock()
	defer auth.mu.Unlock()
	for key, value := range auth.sessions {
		if time.Now().After(value.expires) {
			delete(auth.sessions, key)
		}
	}
	if len(auth.sessions) >= 1024 {
		return "", errors.New("session limit reached")
	}
	if previous, err := request.Cookie("admin_session"); err == nil {
		delete(auth.sessions, previous.Value)
	}
	auth.sessions[token] = session{csrf: csrf, expires: time.Now().Add(12 * time.Hour)}
	setSessionCookie(writer, request, token, 12*60*60)
	return csrf, nil
}

// setSessionCookie emits a host-only strict cookie; TLS is detected without trusting proxy headers.
func setSessionCookie(writer http.ResponseWriter, request *http.Request, token string, maxAge int) {
	http.SetCookie(writer, &http.Cookie{Name: "admin_session", Value: token, Path: "/", HttpOnly: true, Secure: request.TLS != nil, SameSite: http.SameSiteStrictMode, MaxAge: maxAge})
}

// sameOrigin rejects cross-site requests and mismatching Origin authorities/schemes.
// Origin may be absent for non-browser clients, which still require CSRF authentication.
func sameOrigin(request *http.Request) bool {
	if request.Header.Get("Sec-Fetch-Site") == "cross-site" {
		return false
	}
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	if err != nil || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	scheme := "http"
	if request.TLS != nil {
		scheme = "https"
	}
	return parsed.Scheme == scheme && strings.EqualFold(parsed.Host, request.Host)
}

// authorize enforces authentication and CSRF on all non-read API methods.
func (auth *Auth) authorize(writer http.ResponseWriter, request *http.Request) bool {
	value, ok := auth.current(request)
	if !ok {
		apiError(writer, 401, "authentication required")
		return false
	}
	if request.Method != "GET" && request.Method != "HEAD" {
		supplied := request.Header.Get("X-CSRF-Token")
		if !sameOrigin(request) || subtle.ConstantTimeCompare([]byte(supplied), []byte(value.csrf)) != 1 {
			apiError(writer, 403, "invalid CSRF token or origin")
			return false
		}
	}
	return true
}
