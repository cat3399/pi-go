package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func passwordHash(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}

func authTestServer(t *testing.T, password string) *Server {
	t.Helper()
	server, err := New(Options{
		Version: "test", Password: password,
		Application: cursorTestAPI{revision: 0}, AllowedHosts: []string{"example.com"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return server
}

func TestPasswordAuthIssuesCookieAndBearerToken(t *testing.T) {
	server := authTestServer(t, "correct horse")

	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, httptest.NewRequest(http.MethodPost, "/api/v1/not-real", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d", unauthorized.Code)
	}

	login := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
		strings.NewReader(`{"passwordHash":"`+passwordHash("correct horse")+`"}`))
	login.Header.Set("Content-Type", "application/json")
	loginResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(loginResponse, login)
	if loginResponse.Code != http.StatusOK {
		t.Fatalf("login status = %d, body = %s", loginResponse.Code, loginResponse.Body.String())
	}
	var loginBody authStatusResponse
	if err := json.Unmarshal(loginResponse.Body.Bytes(), &loginBody); err != nil {
		t.Fatal(err)
	}
	if !loginBody.Authenticated || loginBody.Token == "" || loginResponse.Header().Get("Set-Cookie") == "" {
		t.Fatalf("login response = %#v, cookie = %q", loginBody, loginResponse.Header().Get("Set-Cookie"))
	}

	authorized := httptest.NewRequest(http.MethodPost, "/api/v1/not-real", nil)
	authorized.Header.Set("Authorization", "Bearer "+loginBody.Token)
	authorizedResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(authorizedResponse, authorized)
	if authorizedResponse.Code != http.StatusNotImplemented {
		t.Fatalf("authorized status = %d, body = %s", authorizedResponse.Code, authorizedResponse.Body.String())
	}
}

func TestPasswordAuthRejectsWrongPasswordThenBacksOff(t *testing.T) {
	server := authTestServer(t, "secret")
	request := func() *http.Request {
		value := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login",
			strings.NewReader(`{"passwordHash":"`+passwordHash("wrong")+`"}`))
		value.RemoteAddr = "192.0.2.1:4321"
		value.Header.Set("Content-Type", "application/json")
		return value
	}

	first := httptest.NewRecorder()
	server.Handler().ServeHTTP(first, request())
	if first.Code != http.StatusUnauthorized {
		t.Fatalf("first status = %d", first.Code)
	}
	second := httptest.NewRecorder()
	server.Handler().ServeHTTP(second, request())
	if second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") == "" {
		t.Fatalf("second response = %d, retry = %q", second.Code, second.Header().Get("Retry-After"))
	}
}

func TestPasswordAuthLogoutRevokesToken(t *testing.T) {
	manager, err := newAuthManager("secret")
	if err != nil {
		t.Fatal(err)
	}
	manager.random = bytes.NewReader(bytes.Repeat([]byte{7}, 32))
	now := time.Date(2026, time.August, 16, 10, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return now }
	token, _, err := manager.issue()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.validate(token); !ok {
		t.Fatal("issued token did not validate")
	}
	manager.revoke(token)
	if _, ok := manager.validate(token); ok {
		t.Fatal("revoked token still validates")
	}
}

func TestPasswordAuthAllowsCredentialedCrossOriginPreflight(t *testing.T) {
	server := authTestServer(t, "secret")
	request := httptest.NewRequest(http.MethodOptions, "/api/v1/snapshot", nil)
	request.Header.Set("Origin", "wails://wails.localhost")
	request.Header.Set("Sec-Fetch-Site", "cross-site")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status = %d, body = %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Access-Control-Allow-Origin"); got != "wails://wails.localhost" {
		t.Fatalf("allow origin = %q", got)
	}
}

func TestPasswordHashPrefixLoadsWithoutKeepingPlaintext(t *testing.T) {
	hash := passwordHash("secret")
	manager, err := newAuthManager(passwordHashPrefix + hash)
	if err != nil {
		t.Fatal(err)
	}
	if !manager.required || hex.EncodeToString(manager.passwordDigest[:]) != hash {
		t.Fatalf("password digest = %x", manager.passwordDigest)
	}
}
