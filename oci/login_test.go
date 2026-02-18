package oci

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
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

func TestExtractBearerParamCaseInsensitive(t *testing.T) {
	t.Parallel()

	header := `bearer Realm="https://auth.example.com/token", Service="registry.example.com", Scope="repository:library/ubuntu:pull"`
	if got := extractBearerParam(header, "realm"); got != "https://auth.example.com/token" {
		t.Fatalf("extractBearerParam(realm) = %q", got)
	}
	if got := extractBearerParam(header, "service"); got != "registry.example.com" {
		t.Fatalf("extractBearerParam(service) = %q", got)
	}
	if got := extractBearerParam(header, "scope"); got != "repository:library/ubuntu:pull" {
		t.Fatalf("extractBearerParam(scope) = %q", got)
	}

	multiHeader := `Basic realm="registry.example.com", Bearer realm="https://auth.example.com/token",service="registry.example.com"`
	if got := extractBearerParam(multiHeader, "realm"); got != "https://auth.example.com/token" {
		t.Fatalf("extractBearerParam(realm, multi challenge) = %q", got)
	}
}

func TestNormalizeRegistry(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "docker-hub", input: "docker.io", want: "index.docker.io"},
		{name: "host-with-scheme-and-path", input: "https://ghcr.io/v2/", want: "ghcr.io"},
		{name: "registry-with-port", input: "localhost:5000", want: "localhost:5000"},
		{name: "empty", input: "", wantErr: true},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := normalizeRegistry(tc.input)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeRegistry(%q): expected error, got nil", tc.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeRegistry(%q): %v", tc.input, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeRegistry(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestResolveAuthEntryCanonicalization(t *testing.T) {
	t.Parallel()

	auths := map[string]dockerAuthEntry{
		"https://docker.io/v2/": {Auth: "legacy-docker"},
		"https://ghcr.io/v2/":   {Auth: "legacy-ghcr"},
	}

	entry, ok := resolveAuthEntry(auths, "index.docker.io")
	if !ok {
		t.Fatalf("resolveAuthEntry did not match docker hub legacy key")
	}
	if !reflect.DeepEqual(entry, dockerAuthEntry{Auth: "legacy-docker"}) {
		t.Fatalf("unexpected docker entry: %#v", entry)
	}

	entry, ok = resolveAuthEntry(auths, "ghcr.io")
	if !ok {
		t.Fatalf("resolveAuthEntry did not match ghcr legacy key")
	}
	if !reflect.DeepEqual(entry, dockerAuthEntry{Auth: "legacy-ghcr"}) {
		t.Fatalf("unexpected ghcr entry: %#v", entry)
	}
}
