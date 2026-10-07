package config

import (
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/cli/cli/v2/internal/keyring"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// findSource returns the source at a 1-based step.
func findSource(t *testing.T, sources []CredentialSource, step int) CredentialSource {
	t.Helper()
	for _, source := range sources {
		if source.Step == step {
			return source
		}
	}
	t.Fatalf("no source at step %d", step)
	return CredentialSource{}
}

func TestResolveCredentialsPlainTextLogin(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	token := "ghp_plaintexttoken0000000000000000000000"
	authCfg := cfg.Authentication().(*AuthConfig)
	_, err := authCfg.Login("github.com", "monalisa", token, "https", false)
	require.NoError(t, err)
	os.Unsetenv("GH_TOKEN")
	os.Unsetenv("GITHUB_TOKEN")
	view := authCfg.ResolveCredentials("github.com")

	require.Equal(t, "monalisa", view.ActiveUser)
	require.Equal(t, 3, view.ActiveStep, "host level config token is selected")
	require.Equal(t, "oauth_token", view.ActiveSource)
	require.Equal(t, len(token), view.ActiveFingerprint.Length)
	require.Equal(t, "ghp_", view.ActiveFingerprint.Prefix)

	require.Len(t, view.Sources, 5)
	require.Equal(t, SourceAbsent, findSource(t, view.Sources, 1).State)
	require.Equal(t, SourceAbsent, findSource(t, view.Sources, 2).State)
	hostConfig := findSource(t, view.Sources, 3)
	require.Equal(t, SourceSelected, hostConfig.State)
	require.Equal(t, CredentialFromConfigFile, hostConfig.Class)
	require.Equal(t, "hosts.github.com.oauth_token", hostConfig.Identifier)
	require.Equal(t, SourceAbsent, findSource(t, view.Sources, 4).State)
	require.Equal(t, SourceAbsent, findSource(t, view.Sources, 5).State)

	require.Len(t, view.Accounts, 1)
	account := view.Accounts[0]
	require.Equal(t, "monalisa", account.Login)
	require.True(t, account.Active)
	require.Equal(t, SourceAbsent, findSource(t, account.Sources, 1).State, "no user keyed keyring entry")
	require.Equal(t, 2, account.EffectiveStep, "TokenForUser falls back to per-user config")
	require.Equal(t, CredentialFromConfigFile, findSource(t, account.Sources, 2).Class)
	require.True(t, account.CommandsUseOwnCredential)
	require.Equal(t, 3, account.CommandsCredentialStep)
	require.Empty(t, account.Conflicts)
	require.Empty(t, view.Conflicts)

	// The view must agree with the existing entry points.
	activeToken, _ := authCfg.ActiveToken("github.com")
	require.Equal(t, token, activeToken)
	tokenForUser, _, err := authCfg.TokenForUser("github.com", "monalisa")
	require.NoError(t, err)
	require.Equal(t, token, tokenForUser)
}

func TestResolveCredentialsSecureLogin(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	token := "gho_securetoken0000000000000000000000000000"
	authCfg := cfg.Authentication().(*AuthConfig)
	insecure, err := authCfg.Login("github.com", "monalisa", token, "https", true)
	require.NoError(t, err)
	require.False(t, insecure)

	view := authCfg.ResolveCredentials("github.com")

	require.Equal(t, 4, view.ActiveStep, "active user keyring entry is selected")
	require.Equal(t, "keyring", view.ActiveSource)
	require.Equal(t, SourceShadowed, findSource(t, view.Sources, 5).State,
		"the unkeyed copy still exists and is covered by the user entry")
	require.Equal(t, SourceAbsent, findSource(t, view.Sources, 3).State)

	account := view.Accounts[0]
	require.Equal(t, 1, account.EffectiveStep)
	userKeyring := findSource(t, account.Sources, 1)
	require.Equal(t, SourceSelected, userKeyring.State)
	require.Equal(t, "monalisa", userKeyring.Account)
	require.Equal(t, SourceAbsent, findSource(t, account.Sources, 2).State)
	require.True(t, account.CommandsUseOwnCredential)
	require.Empty(t, account.Conflicts)
}

func TestResolveCredentialsEnvironmentCoversStoredCredentials(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	stored := "gho_storedtoken0000000000000000000000000000"
	authCfg := cfg.Authentication().(*AuthConfig)
	_, err := authCfg.Login("github.com", "monalisa", stored, "https", true)
	require.NoError(t, err)

	os.Unsetenv("GITHUB_TOKEN")
	envToken := "ghp_envtoken00000000000000000000000000000"
	t.Setenv("GH_TOKEN", envToken)

	view := authCfg.ResolveCredentials("github.com")

	require.Equal(t, 1, view.ActiveStep)
	require.Equal(t, "GH_TOKEN", view.ActiveSource)
	for _, step := range []int{4, 5} {
		source := findSource(t, view.Sources, step)
		require.Equal(t, SourceShadowed, source.State)
		require.Contains(t, source.Reason, "covered by step 1")
		require.Equal(t, len(stored), source.Fingerprint.Length)
	}
	require.Equal(t, SourceAbsent, findSource(t, view.Sources, 2).State)

	// TokenForUser ignores the environment, so the account credential stays effective
	// for `gh auth token --user monalisa` even though commands do not use it.
	account := view.Accounts[0]
	require.Equal(t, 1, account.EffectiveStep)
	require.False(t, account.CommandsUseOwnCredential)
	require.Equal(t, 1, account.CommandsCredentialStep)
	require.Empty(t, account.Conflicts, "shadowing by design is not a conflict")

	activeToken, activeSource := cfg.Authentication().ActiveToken("github.com")
	require.Equal(t, envToken, activeToken)
	require.Equal(t, "GH_TOKEN", activeSource)
}

func TestResolveCredentialsEnvironmentPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		host      string
		env       map[string]string
		wantFirst string
	}{
		{
			name:      "github.com prefers GH_TOKEN over GITHUB_TOKEN",
			host:      "github.com",
			env:       map[string]string{"GH_TOKEN": "ghp_first0000000000000000000000000000000", "GITHUB_TOKEN": "ghp_second000000000000000000000000000000"},
			wantFirst: "GH_TOKEN",
		},
		{
			name:      "enterprise prefers GH_ENTERPRISE_TOKEN",
			host:      "ghe.example.com",
			env:       map[string]string{"GH_ENTERPRISE_TOKEN": "ghp_first0000000000000000000000000000000", "GITHUB_ENTERPRISE_TOKEN": "ghp_second000000000000000000000000000000"},
			wantFirst: "GH_ENTERPRISE_TOKEN",
		},
		{
			name:      "empty GH_TOKEN falls through to GITHUB_TOKEN",
			host:      "github.com",
			env:       map[string]string{"GH_TOKEN": "", "GITHUB_TOKEN": "ghp_second000000000000000000000000000000"},
			wantFirst: "GITHUB_TOKEN",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, _ := NewIsolatedTestConfig(t, "")
			_, err := cfg.Authentication().Login(tt.host, "monalisa", "ghp_stored0000000000000000000000000000000", "https", false)
			require.NoError(t, err)
			for name, value := range tt.env {
				t.Setenv(name, value)
			}

			view := cfg.Authentication().(*AuthConfig).ResolveCredentials(tt.host)
			require.Equal(t, tt.wantFirst, view.Sources[view.ActiveStep-1].Location)
			require.Equal(t, tt.wantFirst, view.ActiveSource)

			first := view.Sources[0]
			if tt.env[first.Identifier] == "" {
				require.Equal(t, SourceEmpty, first.State)
			} else {
				require.Equal(t, SourceSelected, first.State)
			}
			// The stored host config token is covered whenever an env var wins.
			require.Equal(t, SourceShadowed, findSource(t, view.Sources, 3).State)
		})
	}
}

func TestResolveCredentialsDivergentDuplicateCredentials(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	oldToken := "gho_oldtoken000000000000000000000000000000"
	authCfg := cfg.Authentication().(*AuthConfig)
	// A secure login leaves a user keyed keyring entry plus an unkeyed copy.
	_, err := authCfg.Login("github.com", "monalisa", oldToken, "https", true)
	require.NoError(t, err)

	// Simulate a later plain text refresh: new value is written per-user in the
	// config and copied into the host level active slot, while the old keyring
	// entry is left behind.
	newToken := "ghp_newtoken000000000000000000000000000000"
	cfg.cfg.Set([]string{hostsKey, "github.com", usersKey, "monalisa", oauthTokenKey}, newToken)
	cfg.cfg.Set([]string{hostsKey, "github.com", oauthTokenKey}, newToken)

	view := authCfg.ResolveCredentials("github.com")

	// Commands deterministically take the host level plain text refresh.
	require.Equal(t, 3, view.ActiveStep)
	activeToken, _ := authCfg.ActiveToken("github.com")
	require.Equal(t, newToken, activeToken)

	account := view.Accounts[0]
	require.Equal(t, 1, account.EffectiveStep, "TokenForUser prefers the stale keyring entry")
	tokenForUser, _, err := authCfg.TokenForUser("github.com", "monalisa")
	require.NoError(t, err)
	require.Equal(t, oldToken, tokenForUser)
	require.False(t, account.CommandsUseOwnCredential)

	var duplicate, mismatch *CredentialConflict
	for i := range account.Conflicts {
		switch account.Conflicts[i].Type {
		case ConflictDuplicateCredentials:
			duplicate = &account.Conflicts[i]
		case ConflictActiveCredentialMismatch:
			mismatch = &account.Conflicts[i]
		}
	}
	require.NotNil(t, duplicate, "two differing stored credentials are reported")
	require.Equal(t, 1, duplicate.WinnerStep)
	require.Equal(t, 2, duplicate.LoserStep)
	require.NotNil(t, mismatch, "the active resolution divergence is reported")
	require.Equal(t, 3, mismatch.WinnerStep)
	require.Equal(t, 1, mismatch.LoserStep)
}

func TestResolveCredentialsInaccessibleSecureStorage(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	token := "ghp_plaintexttoken0000000000000000000000"
	authCfg := cfg.Authentication().(*AuthConfig)
	_, err := authCfg.Login("github.com", "monalisa", token, "https", false)
	require.NoError(t, err)

	keyring.MockInitWithError(errors.New("keyring unavailable"))

	view := authCfg.ResolveCredentials("github.com")

	require.Equal(t, 3, view.ActiveStep, "the host still resolves from plain text config")
	require.Equal(t, "oauth_token", view.ActiveSource)
	for _, step := range []int{4, 5} {
		source := findSource(t, view.Sources, step)
		require.Equal(t, SourceInaccessible, source.State)
		require.Equal(t, "keyring unavailable", source.AccessError)
	}

	account := view.Accounts[0]
	require.Equal(t, SourceInaccessible, findSource(t, account.Sources, 1).State)
	require.Equal(t, 2, account.EffectiveStep, "TokenForUser falls through to config")
	require.True(t, account.CommandsUseOwnCredential)
	require.Empty(t, account.Conflicts)
}

func TestResolveCredentialsUnkeyedLegacyEntryWithoutActiveUser(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	token := "ghp_legacytoken0000000000000000000000000"
	require.NoError(t, keyring.Set(keyringServiceName("ghe.example.com"), "", token))
	// A host section with no active user or stored accounts.
	cfg.cfg.Set([]string{hostsKey, "ghe.example.com", gitProtocolKey}, "https")

	view := cfg.Authentication().(*AuthConfig).ResolveCredentials("ghe.example.com")

	require.Equal(t, "", view.ActiveUser)
	require.Equal(t, 5, view.ActiveStep)
	require.Equal(t, "keyring", view.ActiveSource)
	require.Equal(t, SourceUnavailable, findSource(t, view.Sources, 4).State)
	unkeyed := findSource(t, view.Sources, 5)
	require.Equal(t, SourceSelected, unkeyed.State)
	require.Equal(t, "", unkeyed.Account)
	require.Empty(t, view.Accounts)
	require.Empty(t, view.Conflicts)

	activeToken, _ := cfg.Authentication().ActiveToken("ghe.example.com")
	require.Equal(t, token, activeToken)
}

func TestResolveCredentialsMultipleAccountsOrdering(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	authCfg := cfg.Authentication().(*AuthConfig)
	_, err := authCfg.Login("github.com", "zoe", "ghp_zoe0000000000000000000000000000000000", "https", false)
	require.NoError(t, err)
	_, err = authCfg.Login("github.com", "abby", "ghp_abby000000000000000000000000000000000", "https", false)
	require.NoError(t, err)

	view := authCfg.ResolveCredentials("github.com")

	require.Equal(t, "abby", view.ActiveUser)
	require.Len(t, view.Accounts, 2)
	require.Equal(t, "abby", view.Accounts[0].Login, "active account first")
	require.True(t, view.Accounts[0].Active)
	require.Equal(t, "zoe", view.Accounts[1].Login)
	require.False(t, view.Accounts[1].Active)
	require.False(t, view.Accounts[1].CommandsUseOwnCredential)
	require.Zero(t, view.Accounts[1].CommandsCredentialStep)
}

func TestResolveCredentialsHostWithNoCredentials(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	cfg.cfg.Set([]string{hostsKey, "ghe.example.com", gitProtocolKey}, "https")

	view := cfg.Authentication().(*AuthConfig).ResolveCredentials("ghe.example.com")

	require.Zero(t, view.ActiveStep)
	require.Equal(t, "default", view.ActiveSource)
	for _, source := range view.Sources {
		require.NotEqual(t, SourceSelected, source.State)
	}
	require.Empty(t, view.Conflicts)
}

func TestResolveCredentialsNeverSerializesTokenContent(t *testing.T) {
	cfg, _ := NewIsolatedTestConfig(t, "")
	token := "ghp_secrettoken00000000000000000000000000"
	authCfg := cfg.Authentication().(*AuthConfig)
	_, err := authCfg.Login("github.com", "monalisa", token, "https", false)
	require.NoError(t, err)
	t.Setenv("GH_TOKEN", "ghp_envsecret00000000000000000000000000000")

	view := authCfg.ResolveCredentials("github.com")
	data, err := json.Marshal(view)
	require.NoError(t, err)
	serialized := string(data)
	assert.NotContains(t, serialized, token)
	assert.NotContains(t, serialized, "ghp_envsecret")
	assert.Contains(t, serialized, `"length"`)
	assert.Contains(t, serialized, `"prefix":"ghp_"`)
}
