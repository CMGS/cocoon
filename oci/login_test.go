package oci

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestVerifyBearerCredentialsSuccess(t *testing.T) {
	t.Parallel()

	const (
		user = "alice"
		pass = "secret"
	)

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUser, gotPass, _ := r.BasicAuth()
		if gotUser == user && gotPass == pass {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "token-ok"})
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(tokenSrv.Close)

	challenge := `Bearer realm="` + tokenSrv.URL + `",service="registry.example.com",scope="repository:library/ubuntu:pull"`
	if err := verifyBearerCredentials(context.Background(), challenge, user, pass); err != nil {
		t.Fatalf("verifyBearerCredentials returned error: %v", err)
	}
}

func TestVerifyBearerCredentialsInvalidCredentials(t *testing.T) {
	t.Parallel()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(tokenSrv.Close)

	challenge := `Bearer realm="` + tokenSrv.URL + `",service="registry.example.com"`
	err := verifyBearerCredentials(context.Background(), challenge, "alice", "bad")
	if err == nil {
		t.Fatalf("expected error for invalid credentials, got nil")
	}
	if !strings.Contains(err.Error(), "invalid credentials") {
		t.Fatalf("expected invalid credentials error, got: %v", err)
	}
}

func TestVerifyBearerCredentialsRejectsAmbiguousAnonymousFlow(t *testing.T) {
	t.Parallel()

	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "anonymous-or-auth"})
	}))
	t.Cleanup(tokenSrv.Close)

	challenge := `Bearer realm="` + tokenSrv.URL + `",service="registry.example.com"`
	err := verifyBearerCredentials(context.Background(), challenge, "alice", "secret")
	if err == nil {
		t.Fatalf("expected error for ambiguous anonymous flow, got nil")
	}
	if !strings.Contains(err.Error(), "also issues tokens for invalid credentials") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestBuildBearerTokenURL(t *testing.T) {
	t.Parallel()

	tokenURL, err := buildBearerTokenURL(
		"https://auth.example.com/token?existing=1",
		"registry.example.com",
		"repository:library/ubuntu:pull",
	)
	if err != nil {
		t.Fatalf("buildBearerTokenURL: %v", err)
	}

	u, err := url.Parse(tokenURL)
	if err != nil {
		t.Fatalf("parse token URL: %v", err)
	}
	q := u.Query()
	if q.Get("existing") != "1" {
		t.Fatalf("expected existing query param to be preserved, got %q", q.Get("existing"))
	}
	if q.Get("service") != "registry.example.com" {
		t.Fatalf("unexpected service query value: %q", q.Get("service"))
	}
	if q.Get("scope") != "repository:library/ubuntu:pull" {
		t.Fatalf("unexpected scope query value: %q", q.Get("scope"))
	}
}
