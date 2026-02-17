package oci

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

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

// Login stores registry credentials in ~/.cocoon/config.json and verifies
// them by pinging the registry.
func Login(ctx context.Context, registry, username, password string) error {
	// Verify credentials by pinging the registry's /v2/ endpoint.
	if err := pingRegistry(ctx, registry, username, password); err != nil {
		return fmt.Errorf("registry authentication failed: %w", err)
	}

	configPath, err := cocoonConfigPath()
	if err != nil {
		return err
	}

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
	cfg.Auths[registry] = dockerAuthEntry{Auth: encoded}

	// Marshal and write atomically with restrictive permissions.
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	dir := filepath.Dir(configPath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create config directory: %w", err)
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("ping %s: %w", registry, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	// 200 or 401 with a Www-Authenticate header (token flow) are both OK for
	// basic credential verification. We only reject outright failures.
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		// Token-based registries return 401 with a Bearer challenge on /v2/.
		// This is normal — credentials will be exchanged for a token at push time.
		if strings.Contains(resp.Header.Get("Www-Authenticate"), "Bearer") {
			return nil
		}
		return fmt.Errorf("invalid credentials for %s (HTTP 401)", registry)
	}
	// Some registries return 403 for bad credentials.
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("access denied for %s (HTTP 403)", registry)
	}

	return nil
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

	registry := target.RegistryStr()
	entry, ok := cfg.Auths[registry]
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
