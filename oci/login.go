package oci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"

	cflock "github.com/CMGS/cocoon/lock/flock"
)

const registryHTTPTimeout = 30 * time.Second

var registryHTTPClient = &http.Client{
	Timeout: registryHTTPTimeout,
}

// cocoonConfigPath returns the path to ~/.cocoon/config.json.
func cocoonConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("get home directory: %w", err)
	}
	return filepath.Join(home, ".cocoon", "config.json"), nil
}

// dockerAuthConfig mirrors Docker's config.json format for the auths section.
type dockerAuthConfig struct {
	Auths map[string]dockerAuthEntry `json:"auths"`
}

type dockerAuthEntry struct {
	Auth string `json:"auth"`
}

func normalizeRegistry(registry string) (string, error) {
	registry = strings.TrimSpace(registry)
	if registry == "" {
		return "", fmt.Errorf("registry cannot be empty")
	}
	if strings.Contains(registry, "://") {
		u, err := url.Parse(registry)
		if err != nil {
			return "", fmt.Errorf("invalid registry %q: %w", registry, err)
		}
		if u.Host == "" {
			return "", fmt.Errorf("invalid registry %q: missing host", registry)
		}
		registry = u.Host
	}
	registry = strings.TrimSuffix(registry, "/")
	if slash := strings.IndexByte(registry, '/'); slash >= 0 {
		registry = registry[:slash]
	}
	reg, err := name.NewRegistry(registry)
	if err != nil {
		return "", fmt.Errorf("invalid registry %q: %w", registry, err)
	}
	return reg.RegistryStr(), nil
}

// Login stores registry credentials in ~/.cocoon/config.json and verifies
// them by pinging the registry.
func Login(ctx context.Context, registry, username, password string) error {
	normalizedRegistry, err := normalizeRegistry(registry)
	if err != nil {
		return err
	}

	// Verify credentials by pinging the registry's /v2/ endpoint.
	if err = pingRegistry(ctx, normalizedRegistry, username, password); err != nil {
		return fmt.Errorf("registry authentication failed: %w", err)
	}

	configPath, err := cocoonConfigPath()
	if err != nil {
		return err
	}

	dir := filepath.Dir(configPath)
	if mkdirErr := os.MkdirAll(dir, 0o700); mkdirErr != nil {
		return fmt.Errorf("create config directory: %w", mkdirErr)
	}

	// Lock to prevent concurrent Login calls from losing each other's credentials.
	lockPath := configPath + ".lock"
	fl := cflock.New(lockPath)
	if lockErr := fl.Lock(); lockErr != nil {
		return fmt.Errorf("acquire config lock: %w", lockErr)
	}
	defer fl.Unlock() //nolint:errcheck

	// Read existing config or create empty.
	cfg := &dockerAuthConfig{Auths: make(map[string]dockerAuthEntry)}
	if data, readErr := os.ReadFile(configPath); readErr == nil { //nolint:gosec // G304: path is derived from user home dir
		// Best-effort parse; if corrupt we overwrite.
		_ = json.Unmarshal(data, cfg)
		if cfg.Auths == nil {
			cfg.Auths = make(map[string]dockerAuthEntry)
		}
	}

	// Encode credentials.
	encoded := base64.StdEncoding.EncodeToString([]byte(username + ":" + password))
	cfg.Auths[normalizedRegistry] = dockerAuthEntry{Auth: encoded}

	// Marshal and write atomically with restrictive permissions.
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	tmpFile := configPath + ".tmp"
	if err := os.WriteFile(tmpFile, append(data, '\n'), 0o600); err != nil {
		return fmt.Errorf("write config: %w", err)
	}
	if err := os.Rename(tmpFile, configPath); err != nil {
		os.Remove(tmpFile) //nolint:errcheck,gosec // G104: best-effort cleanup
		return fmt.Errorf("rename config: %w", err)
	}

	return nil
}

// pingRegistry verifies credentials against a registry's /v2/ endpoint.
func pingRegistry(ctx context.Context, registry, username, password string) error {
	reg, err := name.NewRegistry(registry)
	if err != nil {
		return fmt.Errorf("invalid registry %q: %w", registry, err)
	}

	url := fmt.Sprintf("%s://%s/v2/", reg.Scheme(), reg.RegistryStr())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.SetBasicAuth(username, password)

	resp, err := registryHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("ping %s: %w", registry, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// 200 means direct basic auth succeeded.
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		wwwAuth := resp.Header.Get("Www-Authenticate")
		if hasBearerChallenge(wwwAuth) {
			// Token-based registries (Docker Hub, ghcr.io) return 401 with a Bearer
			// challenge on /v2/. Perform token exchange to verify credentials.
			if err := verifyBearerCredentials(ctx, wwwAuth, username, password); err != nil {
				return err
			}
			return nil
		}
		return fmt.Errorf("invalid credentials for %s (HTTP 401)", registry)
	}
	// Some registries return 403 for bad credentials.
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("access denied for %s (HTTP 403)", registry)
	}

	// Treat server errors as transient failures — do not report login success.
	if resp.StatusCode >= 500 {
		return fmt.Errorf("registry %s returned server error (HTTP %d)", registry, resp.StatusCode)
	}

	// Any other unexpected status code (3xx, other 4xx) should not be treated
	// as a login success. Report the status to avoid false positives.
	return fmt.Errorf("registry %s returned unexpected status (HTTP %d)", registry, resp.StatusCode)
}

// verifyBearerCredentials parses a Bearer Www-Authenticate challenge header and
// performs a token exchange to verify the provided credentials.
// Header format: Bearer realm="https://auth.example.com/token",service="registry.example.com",scope="..."
func verifyBearerCredentials(ctx context.Context, wwwAuth, username, password string) error {
	realm := extractBearerParam(wwwAuth, "realm")
	if realm == "" {
		// No realm found — cannot verify credentials via token exchange.
		return fmt.Errorf("bearer challenge has no realm; cannot verify credentials")
	}

	service := extractBearerParam(wwwAuth, "service")
	scope := extractBearerParam(wwwAuth, "scope")

	tokenURL, err := buildBearerTokenURL(realm, service, scope)
	if err != nil {
		return fmt.Errorf("build bearer token URL: %w", err)
	}

	status, token, err := requestBearerToken(ctx, tokenURL, username, password)
	if err != nil {
		return fmt.Errorf("token exchange request to %s: %w", realm, err)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return fmt.Errorf("invalid credentials: token exchange returned HTTP %d", status)
	}
	if status != http.StatusOK {
		return fmt.Errorf("token exchange returned unexpected status HTTP %d", status)
	}
	if token == "" {
		return fmt.Errorf("token exchange returned HTTP 200 but no token in response")
	}

	// Detect registries that issue bearer tokens even for invalid credentials.
	// In that case, a 200 response does not prove the supplied username/password.
	probeUser := username + "__cocoon_invalid__"
	probePass := password + "__cocoon_invalid__"
	probeStatus, _, probeErr := requestBearerToken(ctx, tokenURL, probeUser, probePass)
	if probeErr != nil {
		return fmt.Errorf("invalid-credential probe request to %s: %w", realm, probeErr)
	}
	if probeStatus == http.StatusOK {
		return fmt.Errorf("could not verify credentials: registry also issues tokens for invalid credentials")
	}
	if probeStatus != http.StatusUnauthorized && probeStatus != http.StatusForbidden {
		return fmt.Errorf("could not verify credentials: invalid-credential probe returned HTTP %d", probeStatus)
	}
	return nil
}

func buildBearerTokenURL(realm, service, scope string) (string, error) {
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if service != "" {
		q.Set("service", service)
	}
	if scope != "" {
		q.Set("scope", scope)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func requestBearerToken(ctx context.Context, tokenURL, username, password string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, tokenURL, nil)
	if err != nil {
		return 0, "", fmt.Errorf("build token request: %w", err)
	}
	req.SetBasicAuth(username, password)

	resp, err := registryHTTPClient.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, "", nil
	}

	var payload struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return 0, "", fmt.Errorf("decode token response: %w", err)
	}
	token := strings.TrimSpace(payload.AccessToken)
	if token == "" {
		token = strings.TrimSpace(payload.Token)
	}
	return resp.StatusCode, token, nil
}

func hasBearerChallenge(header string) bool {
	return strings.Contains(strings.ToLower(header), "bearer ")
}

func splitAuthParams(params string) []string {
	var out []string
	var current strings.Builder
	inQuotes := false
	for i := 0; i < len(params); i++ {
		ch := params[i]
		switch ch {
		case '"':
			inQuotes = !inQuotes
			current.WriteByte(ch)
		case ',':
			if inQuotes {
				current.WriteByte(ch)
				continue
			}
			part := strings.TrimSpace(current.String())
			if part != "" {
				out = append(out, part)
			}
			current.Reset()
		default:
			current.WriteByte(ch)
		}
	}
	last := strings.TrimSpace(current.String())
	if last != "" {
		out = append(out, last)
	}
	return out
}

// extractBearerParam extracts a parameter value from a Bearer Www-Authenticate header.
// E.g., extractBearerParam(`Bearer realm="https://example.com/token",service="reg"`, "realm")
// returns "https://example.com/token".
func extractBearerParam(header, param string) string {
	bearerIdx := strings.Index(strings.ToLower(header), "bearer ")
	if bearerIdx < 0 {
		return ""
	}
	header = strings.TrimSpace(header[bearerIdx:])
	space := strings.IndexAny(header, " \t")
	if space < 0 || space+1 >= len(header) {
		return ""
	}
	for _, token := range splitAuthParams(header[space+1:]) {
		key, value, ok := strings.Cut(token, "=")
		if !ok {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(key), param) {
			continue
		}
		value = strings.TrimSpace(value)
		return strings.Trim(value, "\"")
	}
	return ""
}

// cocoonKeychain is an authn.Keychain that reads from ~/.cocoon/config.json.
type cocoonKeychain struct{}

// CocoonKeychain returns an authn.Keychain that resolves credentials from
// Cocoon's own config file (~/.cocoon/config.json).
func CocoonKeychain() authn.Keychain {
	return &cocoonKeychain{}
}

// Resolve implements authn.Keychain.
func (k *cocoonKeychain) Resolve(target authn.Resource) (authn.Authenticator, error) {
	configPath, err := cocoonConfigPath()
	if err != nil {
		return authn.Anonymous, nil
	}

	data, err := os.ReadFile(configPath) //nolint:gosec // G304: path is derived from user home dir
	if err != nil {
		return authn.Anonymous, nil
	}

	var cfg dockerAuthConfig
	if err = json.Unmarshal(data, &cfg); err != nil {
		return authn.Anonymous, nil
	}

	entry, ok := resolveAuthEntry(cfg.Auths, target.RegistryStr())
	if !ok {
		return authn.Anonymous, nil
	}

	decoded, err := base64.StdEncoding.DecodeString(entry.Auth)
	if err != nil {
		return authn.Anonymous, nil
	}

	parts := splitOnce(string(decoded), ":")
	if len(parts) != 2 {
		return authn.Anonymous, nil
	}

	return &authn.Basic{
		Username: parts[0],
		Password: parts[1],
	}, nil
}

func resolveAuthEntry(auths map[string]dockerAuthEntry, registry string) (dockerAuthEntry, bool) {
	if auths == nil {
		return dockerAuthEntry{}, false
	}
	if entry, ok := auths[registry]; ok {
		return entry, true
	}
	normalizedRegistry, err := normalizeRegistry(registry)
	if err != nil {
		return dockerAuthEntry{}, false
	}

	// Fast-path docker hub aliases (handles legacy configs).
	if normalizedRegistry == "index.docker.io" {
		for _, alias := range []string{"docker.io", "registry-1.docker.io", "https://index.docker.io/v1/"} {
			if entry, ok := auths[alias]; ok {
				return entry, true
			}
		}
	}

	for key, entry := range auths {
		keyRegistry, keyErr := normalizeRegistry(key)
		if keyErr != nil {
			continue
		}
		if keyRegistry == normalizedRegistry {
			return entry, true
		}
	}
	return dockerAuthEntry{}, false
}

// splitOnce splits s on the first occurrence of sep, returning at most 2 parts.
func splitOnce(s, sep string) []string {
	i := 0
	for i < len(s) {
		if i+len(sep) <= len(s) && s[i:i+len(sep)] == sep {
			return []string{s[:i], s[i+len(sep):]}
		}
		i++
	}
	return []string{s}
}
