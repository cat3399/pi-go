package web

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	authCookieName     = "pi_go_auth"
	passwordHashPrefix = "sha256-v1:"
	authTokenVersion   = "v1"
	authTokenPurpose   = "pi-go-web-auth-v1\x00"
	defaultAuthTTL     = 365 * 24 * time.Hour
	maxLoginBodyBytes  = 8 << 10
)

type authManager struct {
	required       bool
	passwordDigest [sha256.Size]byte
	ttl            time.Duration
	now            func() time.Time

	mu       sync.Mutex
	attempts map[string]loginAttempt
}

type loginAttempt struct {
	failures    int
	lastFailure time.Time
	nextAttempt time.Time
	lockedUntil time.Time
}

type authStatusResponse struct {
	OK            bool   `json:"ok"`
	AuthRequired  bool   `json:"authRequired"`
	Authenticated bool   `json:"authenticated"`
	Token         string `json:"token,omitempty"`
	ExpiresAtMS   *int64 `json:"expiresAtMs"`
	TTLMS         *int64 `json:"ttlMs"`
	Error         string `json:"error,omitempty"`
	RetryAfterMS  int64  `json:"retryAfterMs,omitempty"`
}

func newAuthManager(password string) (*authManager, error) {
	manager := &authManager{
		ttl: defaultAuthTTL, now: time.Now,
		attempts: make(map[string]loginAttempt),
	}
	password = strings.TrimSpace(password)
	if password == "" {
		return manager, nil
	}
	manager.required = true
	if strings.HasPrefix(password, passwordHashPrefix) {
		value := strings.TrimSpace(strings.TrimPrefix(password, passwordHashPrefix))
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size {
			return nil, errors.New("password hash must be sha256-v1 followed by 64 hexadecimal characters")
		}
		copy(manager.passwordDigest[:], decoded)
		return manager, nil
	}
	manager.passwordDigest = sha256.Sum256([]byte(password))
	return manager, nil
}

func (a *authManager) registerRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/auth/status", a.handleStatus)
	mux.HandleFunc("POST /api/v1/auth/login", a.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", a.handleLogout)
}

func (a *authManager) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !a.required || !strings.HasPrefix(request.URL.Path, "/api/v1/") || authPublicPath(request.URL.Path) {
			next.ServeHTTP(writer, request)
			return
		}
		token := authTokenFromRequest(request)
		expiresAt, ok := a.validate(token)
		if !ok {
			writer.Header().Set("WWW-Authenticate", "Bearer")
			writeJSON(writer, http.StatusUnauthorized, map[string]any{"ok": false, "error": "Unauthorized"})
			return
		}
		setAuthCookie(writer, request, token, expiresAt)
		next.ServeHTTP(writer, request)
	})
}

func authPublicPath(path string) bool {
	switch path {
	case "/api/v1/auth/status", "/api/v1/auth/login", "/api/v1/auth/logout",
		"/api/v1/health", "/api/v1/capabilities":
		return true
	default:
		return false
	}
}

func (a *authManager) handleStatus(writer http.ResponseWriter, request *http.Request) {
	if !a.required {
		writeJSON(writer, http.StatusOK, authStatusResponse{
			OK: true, AuthRequired: false, Authenticated: true,
		})
		return
	}
	token := authTokenFromRequest(request)
	expiresAt, authenticated := a.validate(token)
	if authenticated {
		setAuthCookie(writer, request, token, expiresAt)
	}
	writeJSON(writer, http.StatusOK, a.statusResponse(authenticated, "", expiresAt))
}

func (a *authManager) handleLogin(writer http.ResponseWriter, request *http.Request) {
	if !a.required {
		writeJSON(writer, http.StatusOK, authStatusResponse{
			OK: true, AuthRequired: false, Authenticated: true,
		})
		return
	}
	client := loginClientKey(request)
	if retry := a.retryAfter(client); retry > 0 {
		writeLoginLimited(writer, retry)
		return
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maxLoginBodyBytes)
	var input struct {
		PasswordHash string `json:"passwordHash"`
	}
	decoder := json.NewDecoder(request.Body)
	if err := decoder.Decode(&input); err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"ok": false, "error": "Invalid login request"})
		return
	}
	digest, err := hex.DecodeString(strings.TrimSpace(input.PasswordHash))
	valid := err == nil && len(digest) == sha256.Size &&
		subtle.ConstantTimeCompare(digest, a.passwordDigest[:]) == 1
	if !valid {
		retry, locked := a.recordFailure(client)
		if locked {
			writeLoginLimited(writer, retry)
			return
		}
		writeJSON(writer, http.StatusUnauthorized, map[string]any{
			"ok": false, "authRequired": true, "authenticated": false, "error": "Invalid password",
		})
		return
	}
	a.recordSuccess(client)
	token, expiresAt := a.issue()
	setAuthCookie(writer, request, token, expiresAt)
	response := a.statusResponse(true, token, expiresAt)
	writeJSON(writer, http.StatusOK, response)
}

func (a *authManager) handleLogout(writer http.ResponseWriter, request *http.Request) {
	clearAuthCookie(writer, request)
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true})
}

func (a *authManager) statusResponse(authenticated bool, token string, expiresAt time.Time) authStatusResponse {
	ttl := a.ttl.Milliseconds()
	response := authStatusResponse{
		OK: true, AuthRequired: a.required, Authenticated: authenticated, Token: token, TTLMS: &ttl,
	}
	if authenticated {
		milliseconds := expiresAt.UnixMilli()
		response.ExpiresAtMS = &milliseconds
	}
	return response
}

func (a *authManager) issue() (string, time.Time) {
	expiresAt := a.now().Add(a.ttl).Truncate(time.Second)
	var expiry [8]byte
	binary.BigEndian.PutUint64(expiry[:], uint64(expiresAt.Unix()))
	signature := a.sign(expiry[:])
	token := authTokenVersion + "." + base64.RawURLEncoding.EncodeToString(expiry[:]) + "." +
		base64.RawURLEncoding.EncodeToString(signature)
	return token, expiresAt
}

func (a *authManager) validate(token string) (time.Time, bool) {
	if !a.required {
		return time.Time{}, true
	}
	if token == "" {
		return time.Time{}, false
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != authTokenVersion {
		return time.Time{}, false
	}
	expiry, expiryErr := base64.RawURLEncoding.DecodeString(parts[1])
	signature, signatureErr := base64.RawURLEncoding.DecodeString(parts[2])
	if expiryErr != nil || signatureErr != nil || len(expiry) != 8 || len(signature) != sha256.Size {
		return time.Time{}, false
	}
	expiresAt := time.Unix(int64(binary.BigEndian.Uint64(expiry)), 0)
	if !expiresAt.After(a.now()) || subtle.ConstantTimeCompare(signature, a.sign(expiry)) != 1 {
		return time.Time{}, false
	}
	return expiresAt, true
}

func (a *authManager) sign(expiry []byte) []byte {
	mac := hmac.New(sha256.New, a.passwordDigest[:])
	_, _ = mac.Write([]byte(authTokenPurpose))
	_, _ = mac.Write(expiry)
	return mac.Sum(nil)
}

func (a *authManager) retryAfter(client string) time.Duration {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	attempt, ok := a.attempts[client]
	if !ok {
		return 0
	}
	if now.Sub(attempt.lastFailure) > 10*time.Minute {
		delete(a.attempts, client)
		return 0
	}
	until := attempt.nextAttempt
	if attempt.lockedUntil.After(until) {
		until = attempt.lockedUntil
	}
	if until.After(now) {
		return until.Sub(now)
	}
	return 0
}

func (a *authManager) recordFailure(client string) (time.Duration, bool) {
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	attempt := a.attempts[client]
	if now.Sub(attempt.lastFailure) > 10*time.Minute {
		attempt = loginAttempt{}
	}
	attempt.failures++
	attempt.lastFailure = now
	if attempt.failures >= 10 {
		attempt.lockedUntil = now.Add(15 * time.Minute)
		attempt.nextAttempt = attempt.lockedUntil
		a.attempts[client] = attempt
		return 15 * time.Minute, true
	}
	delay := time.Second << min(attempt.failures-1, 5)
	attempt.nextAttempt = now.Add(delay)
	a.attempts[client] = attempt
	return delay, false
}

func (a *authManager) recordSuccess(client string) {
	a.mu.Lock()
	delete(a.attempts, client)
	a.mu.Unlock()
}

func writeLoginLimited(writer http.ResponseWriter, retry time.Duration) {
	retryMilliseconds := max(int64(1), retry.Milliseconds())
	retrySeconds := max(int64(1), (retryMilliseconds+999)/1000)
	writer.Header().Set("Retry-After", strconv.FormatInt(retrySeconds, 10))
	writeJSON(writer, http.StatusTooManyRequests, authStatusResponse{
		OK: false, AuthRequired: true, Authenticated: false,
		Error: "Too many login attempts", RetryAfterMS: retryMilliseconds,
	})
}

func authTokenFromRequest(request *http.Request) string {
	if token := strings.TrimSpace(request.Header.Get("X-Pi-Go-Token")); token != "" {
		return token
	}
	if authorization := strings.TrimSpace(request.Header.Get("Authorization")); len(authorization) > 7 &&
		strings.EqualFold(authorization[:7], "Bearer ") {
		return strings.TrimSpace(authorization[7:])
	}
	if token := strings.TrimSpace(request.URL.Query().Get("token")); token != "" {
		return token
	}
	if cookie, err := request.Cookie(authCookieName); err == nil {
		return strings.TrimSpace(cookie.Value)
	}
	return ""
}

func setAuthCookie(writer http.ResponseWriter, request *http.Request, token string, expiresAt time.Time) {
	if token == "" {
		return
	}
	http.SetCookie(writer, &http.Cookie{
		Name: authCookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: request.TLS != nil,
		Expires: expiresAt,
	})
}

func clearAuthCookie(writer http.ResponseWriter, request *http.Request) {
	http.SetCookie(writer, &http.Cookie{
		Name: authCookieName, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteLaxMode, Secure: request.TLS != nil,
		MaxAge: -1, Expires: time.Unix(0, 0),
	})
}

func loginClientKey(request *http.Request) string {
	host, _, err := net.SplitHostPort(strings.TrimSpace(request.RemoteAddr))
	if err == nil {
		return strings.TrimPrefix(host, "::ffff:")
	}
	if request.RemoteAddr == "" {
		return "unknown"
	}
	return strings.TrimPrefix(request.RemoteAddr, "::ffff:")
}
