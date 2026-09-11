// Package claude polls the unofficial Claude Code oauth/usage endpoint for
// per-account rate-limit windows, following the internal/cursorusage model:
// a small stdlib net/http client, credentials read fresh from config on
// every poll, a dedicated snapshot table (the generalized
// rate_limit_snapshots table shared with Codex), and no join to sessions.
//
// See docs/internal/session-format-sources.md for the endpoint and
// ~/.claude.json evidence entry, and docs/token-usage.md /
// docs/configuration.md for user-facing documentation. This is an
// unofficial endpoint used by Claude Code itself and may change without
// notice.
package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// Identity is the account identity read from a .claude.json file's
// top-level oauthAccount object, with no network access. Verified against
// a real ~/.claude.json on 2026-09-09 (see
// docs/internal/session-format-sources.md): every field below is present
// for a logged-in Claude Code install.
type Identity struct {
	AccountUUID               string `json:"accountUuid"`
	EmailAddress              string `json:"emailAddress"`
	OrganizationUUID          string `json:"organizationUuid"`
	OrganizationName          string `json:"organizationName"`
	OrganizationType          string `json:"organizationType"`
	SeatTier                  string `json:"seatTier"`
	UserRateLimitTier         string `json:"userRateLimitTier"`
	OrganizationRateLimitTier string `json:"organizationRateLimitTier"`
}

// Label returns a human-readable identity label for the settings panel and
// rate-limit cards: the organization name when present, otherwise the
// account email.
func (id Identity) Label() string {
	if id.OrganizationName != "" {
		return id.OrganizationName
	}
	return id.EmailAddress
}

// AccountKey returns the identity a rate-limit snapshot is grouped and
// deduplicated by (db.RateLimitSnapshot.AccountID). A rate-limit window
// belongs to a seat within an organization, not to the human user alone:
// ~/.claude.json's oauthAccount keeps the same accountUuid across every
// organization a user is a member of, and only organizationUuid /
// organizationName change when `claude` logs out and back into a
// different org (kata 1k4b). Keying solely on AccountUUID collapsed both
// orgs into one LatestRateLimitSnapshots group, so the most recent poll
// silently overwrote the other org's cards on the Usage page. Composing
// "<accountUuid>/<organizationUuid>" keeps both halves readable in a raw
// dedup_key or log line -- it reads as a seat in an org, not an opaque
// hash -- while staying stable poll to poll; organizationUuid alone would
// separate the two orgs just as well, but pairing it with accountUuid
// costs nothing and keeps the key meaningful if a future response shape
// ever reuses an organizationUuid across accountUuids. A missing
// organizationUuid (defensively -- every field is present for a real
// logged-in install per the Identity doc comment) falls back to the bare
// accountUuid rather than an accountUuid-trailing-slash key.
func (id Identity) AccountKey() string {
	if id.OrganizationUUID == "" {
		return id.AccountUUID
	}
	return id.AccountUUID + "/" + id.OrganizationUUID
}

type claudeDotJSON struct {
	OAuthAccount *Identity `json:"oauthAccount"`
}

// DefaultClaudeConfigPath returns the default .claude.json path
// (~/.claude.json) for the given home directory.
func DefaultClaudeConfigPath(homeDir string) string {
	return filepath.Join(homeDir, ".claude.json")
}

// ReadIdentity reads the oauthAccount identity from a .claude.json file at
// path. It performs no network access. A missing oauthAccount key (a
// .claude.json that exists but predates login, or belongs to an API-key /
// non-subscriber install) returns a zero Identity and no error, since the
// file format itself is valid; callers should treat a zero AccountUUID as
// "identity unavailable".
func ReadIdentity(path string) (Identity, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Identity{}, fmt.Errorf("reading claude config %s: %w", path, err)
	}
	var doc claudeDotJSON
	if err := json.Unmarshal(data, &doc); err != nil {
		return Identity{}, fmt.Errorf("parsing claude config %s: %w", path, err)
	}
	if doc.OAuthAccount == nil {
		return Identity{}, nil
	}
	return *doc.OAuthAccount, nil
}
