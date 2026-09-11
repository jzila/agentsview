package claude

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// KeychainService is the macOS Keychain generic-password service name
// Claude Code stores its OAuth credentials under. Verified against the
// Claude Code 2.1.266 bundle (see
// docs/internal/session-format-sources.md): it shells out to
// `security find-generic-password -a "$USER" -w -s "Claude Code-credentials"`.
const KeychainService = "Claude Code-credentials"

// Credentials is the OAuth credential payload read from the macOS
// Keychain or ~/.claude/.credentials.json: the top-level claudeAiOauth
// object. Only AccessToken is used by the poller; the rest is decoded for
// completeness and potential diagnostics, and is never logged or
// persisted.
type Credentials struct {
	AccessToken           string `json:"accessToken"`
	RefreshToken          string `json:"refreshToken"`
	ExpiresAt             int64  `json:"expiresAt"`
	RefreshTokenExpiresAt int64  `json:"refreshTokenExpiresAt,omitempty"`
}

type credentialsFile struct {
	ClaudeAIOAuth *Credentials `json:"claudeAiOauth"`
}

// KeychainReader abstracts reading one generic-password secret so the
// credentials loader can be tested with a fake instead of shelling out to
// the real macOS `security` tool.
type KeychainReader interface {
	Read(ctx context.Context, service, account string) (string, error)
}

// execKeychainReader is the real KeychainReader, backed by the macOS
// `security` CLI. It is a no-op returning an error on any non-macOS
// platform.
type execKeychainReader struct{}

func (execKeychainReader) Read(ctx context.Context, service, account string) (string, error) {
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("keychain credentials are only available on macOS")
	}
	cmd := exec.CommandContext(
		ctx, "security", "find-generic-password",
		"-a", account, "-w", "-s", service,
	)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("reading keychain entry %q: %w", service, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// DefaultKeychainReader is the KeychainReader used outside of tests.
var DefaultKeychainReader KeychainReader = execKeychainReader{}

// CredentialsSource is a parsed `credentials = "..."` config value:
// "keychain", "file:<path>", or "env:<VAR>".
type CredentialsSource struct {
	Kind  string // "keychain", "file", or "env"
	Value string // path for "file", variable name for "env"; empty for "keychain"
}

// ParseCredentialsSource parses one [claude.accounts.NAME] credentials
// value.
func ParseCredentialsSource(raw string) (CredentialsSource, error) {
	raw = strings.TrimSpace(raw)
	switch {
	case raw == "keychain":
		return CredentialsSource{Kind: "keychain"}, nil
	case strings.HasPrefix(raw, "file:"):
		path := strings.TrimSpace(strings.TrimPrefix(raw, "file:"))
		if path == "" {
			return CredentialsSource{}, fmt.Errorf("credentials \"file:\" requires a path")
		}
		return CredentialsSource{Kind: "file", Value: path}, nil
	case strings.HasPrefix(raw, "env:"):
		name := strings.TrimSpace(strings.TrimPrefix(raw, "env:"))
		if name == "" {
			return CredentialsSource{}, fmt.Errorf("credentials \"env:\" requires a variable name")
		}
		return CredentialsSource{Kind: "env", Value: name}, nil
	default:
		return CredentialsSource{}, fmt.Errorf(
			"unrecognized credentials source %q (want \"keychain\", \"file:<path>\", or \"env:<VAR>\")",
			raw,
		)
	}
}

// DefaultLinuxCredentialsPath returns the default Claude Code credentials
// file path on Linux (~/.claude/.credentials.json), where there is no
// system keychain.
func DefaultLinuxCredentialsPath(homeDir string) string {
	return filepath.Join(homeDir, ".claude", ".credentials.json")
}

// ErrNoAccessToken is returned when a credentials source resolves to a
// payload with no (or an expired) access token. The caller should not
// attempt to refresh it -- only `claude` itself refreshes OAuth tokens.
var ErrNoAccessToken = fmt.Errorf("no access token available")

// ResolveAccessToken reads the current OAuth access token for src, reading
// fresh from the underlying source every call (per docs/token-usage.md:
// "read the current token on every poll"). homeDir is the user's home
// directory, used to resolve the default keychain fallback on Linux.
// keychain is the KeychainReader to use for Kind == "keychain"; pass nil
// to use DefaultKeychainReader.
func ResolveAccessToken(
	ctx context.Context, src CredentialsSource, homeDir string, keychain KeychainReader,
) (string, error) {
	if keychain == nil {
		keychain = DefaultKeychainReader
	}
	switch src.Kind {
	case "keychain":
		if runtime.GOOS == "darwin" {
			account := currentUsername()
			raw, err := keychain.Read(ctx, KeychainService, account)
			if err != nil {
				return "", err
			}
			return accessTokenFromCredentialsJSON([]byte(raw))
		}
		// No system keychain on Linux (or elsewhere): fall back to the
		// documented default credentials file location.
		return accessTokenFromFile(DefaultLinuxCredentialsPath(homeDir))
	case "file":
		return accessTokenFromFile(ExpandHome(src.Value, homeDir))
	case "env":
		token := strings.TrimSpace(os.Getenv(src.Value))
		if token == "" {
			return "", fmt.Errorf("%w: environment variable %q is empty", ErrNoAccessToken, src.Value)
		}
		return token, nil
	default:
		return "", fmt.Errorf("unsupported credentials source kind %q", src.Kind)
	}
}

func accessTokenFromFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading credentials file %s: %w", path, err)
	}
	return accessTokenFromCredentialsJSON(data)
}

func accessTokenFromCredentialsJSON(data []byte) (string, error) {
	var doc credentialsFile
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("parsing credentials JSON: %w", err)
	}
	if doc.ClaudeAIOAuth == nil || strings.TrimSpace(doc.ClaudeAIOAuth.AccessToken) == "" {
		return "", ErrNoAccessToken
	}
	if doc.ClaudeAIOAuth.ExpiresAt > 0 {
		expiresAt := time.UnixMilli(doc.ClaudeAIOAuth.ExpiresAt)
		if time.Now().After(expiresAt) {
			return "", fmt.Errorf("%w: token expired at %s", ErrNoAccessToken, expiresAt.Format(time.RFC3339))
		}
	}
	return doc.ClaudeAIOAuth.AccessToken, nil
}

func currentUsername() string {
	if v := strings.TrimSpace(os.Getenv("USER")); v != "" {
		return v
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return ""
}

// ExpandHome expands a leading "~" or "~/" in path against homeDir.
// Exported so every caller that resolves a user-supplied identity or
// credentials path (the poller, the Claude accounts settings panel, and
// `agentsview claude statusline-sink`) expands "~" consistently --
// documented [claude.accounts.NAME] examples use `claude_config =
// "~/..."`, which reaches os.ReadFile literally and fails without this
// (roborev finding on kata 9rs0, filed as kata qs2f).
func ExpandHome(path, homeDir string) string {
	if path == "~" {
		return homeDir
	}
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(homeDir, rest)
	}
	return path
}
