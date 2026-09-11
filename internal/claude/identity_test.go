package claude

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReadIdentity_FullOAuthAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	require.NoError(t, os.WriteFile(path, []byte(`{
		"numStartups": 12,
		"oauthAccount": {
			"accountUuid": "c3691235-4e59-4757-8432-b6d1feec4a05",
			"emailAddress": "person@example.com",
			"organizationUuid": "0de39a59-3a3c-44f5-b45d-d1bec03cbd20",
			"organizationName": "Acme",
			"organizationType": "claude_team",
			"seatTier": "team_tier_1",
			"userRateLimitTier": "default_claude_max_5x",
			"organizationRateLimitTier": "default_raven"
		}
	}`), 0o600))

	id, err := ReadIdentity(path)
	require.NoError(t, err)
	assert.Equal(t, "c3691235-4e59-4757-8432-b6d1feec4a05", id.AccountUUID)
	assert.Equal(t, "person@example.com", id.EmailAddress)
	assert.Equal(t, "Acme", id.OrganizationName)
	assert.Equal(t, "claude_team", id.OrganizationType)
	assert.Equal(t, "team_tier_1", id.SeatTier)
	assert.Equal(t, "default_claude_max_5x", id.UserRateLimitTier)
	assert.Equal(t, "default_raven", id.OrganizationRateLimitTier)
	assert.Equal(t, "Acme", id.Label())
}

func TestReadIdentity_LabelFallsBackToEmail(t *testing.T) {
	id := Identity{EmailAddress: "solo@example.com"}
	assert.Equal(t, "solo@example.com", id.Label())
}

// TestIdentity_AccountKey_DistinctOrganizationsYieldDistinctKeys covers
// kata 1k4b: ~/.claude.json keeps the same accountUuid across every
// organization a user belongs to, so an identity for org A and an
// identity for org B that share an accountUuid must still produce
// distinct AccountKey values, or the two orgs would collapse into one
// LatestRateLimitSnapshots group and the more recent poll would overwrite
// the other org's cards.
func TestIdentity_AccountKey_DistinctOrganizationsYieldDistinctKeys(t *testing.T) {
	orgA := Identity{AccountUUID: "6f9c9b7e-0000-0000-0000-000000000001", OrganizationUUID: "11111111-0000-0000-0000-000000000001"}
	orgB := Identity{AccountUUID: "6f9c9b7e-0000-0000-0000-000000000001", OrganizationUUID: "22222222-0000-0000-0000-000000000002"}

	assert.NotEqual(t, orgA.AccountKey(), orgB.AccountKey())
	assert.Equal(t,
		"6f9c9b7e-0000-0000-0000-000000000001/11111111-0000-0000-0000-000000000001",
		orgA.AccountKey(),
	)
	assert.Equal(t,
		"6f9c9b7e-0000-0000-0000-000000000001/22222222-0000-0000-0000-000000000002",
		orgB.AccountKey(),
	)
}

func TestIdentity_AccountKey_FallsBackToBareAccountUUIDWhenOrganizationUUIDMissing(t *testing.T) {
	id := Identity{AccountUUID: "6f9c9b7e-0000-0000-0000-000000000001"}
	assert.Equal(t, "6f9c9b7e-0000-0000-0000-000000000001", id.AccountKey())
}

func TestReadIdentity_MissingOAuthAccount(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"numStartups": 1}`), 0o600))

	id, err := ReadIdentity(path)
	require.NoError(t, err, "a valid file with no oauthAccount is not an error")
	assert.Empty(t, id.AccountUUID)
}

func TestReadIdentity_MissingFile(t *testing.T) {
	_, err := ReadIdentity(filepath.Join(t.TempDir(), "does-not-exist.json"))
	assert.Error(t, err)
}

func TestReadIdentity_MalformedJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude.json")
	require.NoError(t, os.WriteFile(path, []byte(`not json`), 0o600))

	_, err := ReadIdentity(path)
	assert.Error(t, err)
}
