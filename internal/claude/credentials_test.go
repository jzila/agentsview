package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCredentialsSource(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    CredentialsSource
		wantErr bool
	}{
		{"keychain", "keychain", CredentialsSource{Kind: "keychain"}, false},
		{"file", "file:/home/john/.claude/.credentials.json", CredentialsSource{Kind: "file", Value: "/home/john/.claude/.credentials.json"}, false},
		{"env", "env:CLAUDE_TOKEN", CredentialsSource{Kind: "env", Value: "CLAUDE_TOKEN"}, false},
		{"file missing path", "file:", CredentialsSource{}, true},
		{"env missing name", "env:", CredentialsSource{}, true},
		{"unrecognized", "vault:secret/claude", CredentialsSource{}, true},
		{"empty", "", CredentialsSource{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCredentialsSource(tt.raw)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func writeCredentialsFile(t *testing.T, path string, accessToken string, expiresAt int64) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	body := `{"claudeAiOauth":{"accessToken":"` + accessToken + `","refreshToken":"r","expiresAt":` +
		strconv.FormatInt(expiresAt, 10) + `}}`
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

func TestResolveAccessToken_FileSource(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	writeCredentialsFile(t, path, "the-access-token", 0)

	token, err := ResolveAccessToken(
		context.Background(), CredentialsSource{Kind: "file", Value: path}, dir, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "the-access-token", token)
}

func TestResolveAccessToken_FileSourceExpandsHome(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".claude", ".credentials.json")
	writeCredentialsFile(t, path, "home-token", 0)

	token, err := ResolveAccessToken(
		context.Background(), CredentialsSource{Kind: "file", Value: "~/.claude/.credentials.json"}, dir, nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "home-token", token)
}

func TestResolveAccessToken_FileSourceExpiredToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	writeCredentialsFile(t, path, "expired-token", time.Now().Add(-time.Hour).UnixMilli())

	_, err := ResolveAccessToken(
		context.Background(), CredentialsSource{Kind: "file", Value: path}, dir, nil,
	)
	require.ErrorIs(t, err, ErrNoAccessToken)
}

func TestResolveAccessToken_FileSourceMissingAccessToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	require.NoError(t, os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":""}}`), 0o600))

	_, err := ResolveAccessToken(
		context.Background(), CredentialsSource{Kind: "file", Value: path}, dir, nil,
	)
	require.ErrorIs(t, err, ErrNoAccessToken)
}

func TestResolveAccessToken_EnvSource(t *testing.T) {
	t.Setenv("AGENTSVIEW_TEST_CLAUDE_TOKEN", "env-token")
	token, err := ResolveAccessToken(
		context.Background(), CredentialsSource{Kind: "env", Value: "AGENTSVIEW_TEST_CLAUDE_TOKEN"}, "", nil,
	)
	require.NoError(t, err)
	assert.Equal(t, "env-token", token)
}

func TestResolveAccessToken_EnvSourceEmpty(t *testing.T) {
	t.Setenv("AGENTSVIEW_TEST_CLAUDE_TOKEN_EMPTY", "")
	_, err := ResolveAccessToken(
		context.Background(), CredentialsSource{Kind: "env", Value: "AGENTSVIEW_TEST_CLAUDE_TOKEN_EMPTY"}, "", nil,
	)
	require.ErrorIs(t, err, ErrNoAccessToken)
}

// fakeKeychainReader is the interface-with-a-fake seam for testing the
// macOS Keychain path without shelling out to the real `security` tool.
type fakeKeychainReader struct {
	service, account string
	payload          string
	err              error
}

func (f *fakeKeychainReader) Read(_ context.Context, service, account string) (string, error) {
	f.service = service
	f.account = account
	if f.err != nil {
		return "", f.err
	}
	return f.payload, nil
}

func TestResolveAccessToken_KeychainSourceUsesFake(t *testing.T) {
	fake := &fakeKeychainReader{
		payload: `{"claudeAiOauth":{"accessToken":"keychain-token","refreshToken":"r","expiresAt":0}}`,
	}
	dir := t.TempDir()
	token, err := ResolveAccessToken(
		context.Background(), CredentialsSource{Kind: "keychain"}, dir, fake,
	)
	if runtime.GOOS == "darwin" {
		// ResolveAccessToken only consults the keychain reader on
		// macOS; the fake proves the service/account passed to
		// `security find-generic-password` without shelling out.
		require.NoError(t, err)
		assert.Equal(t, "keychain-token", token)
		assert.Equal(t, KeychainService, fake.service)
		assert.NotEmpty(t, fake.account)
	} else {
		// Non-macOS falls back to the documented Linux default file
		// path, which does not exist in this sandbox, so the fake is
		// never invoked and a read error is expected instead.
		assert.Error(t, err)
		assert.Empty(t, fake.service, "the keychain fake must not be consulted off darwin")
	}
}

func TestResolveAccessToken_KeychainReaderErrorPropagates(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("keychain path only exercised on darwin")
	}
	fake := &fakeKeychainReader{err: assert.AnError}
	_, err := ResolveAccessToken(
		context.Background(), CredentialsSource{Kind: "keychain"}, t.TempDir(), fake,
	)
	require.Error(t, err)
}
