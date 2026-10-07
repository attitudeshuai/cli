package resolve

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/google/shlex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newOpts(t *testing.T, ios *iostreams.IOStreams, cfg gh.Config) *resolveOptions {
	t.Helper()
	return &resolveOptions{
		IO:     ios,
		Config: func() (gh.Config, error) { return cfg, nil },
	}
}

func TestNewCmdResolve(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		wantHost   string
		wantUser   string
		wantJSON   bool
		wantErr    bool
		wantErrMsg string
	}{
		{
			name:     "no flags",
			input:    "",
			wantJSON: false,
		},
		{
			name:     "hostname",
			input:    "--hostname github.example.com",
			wantHost: "github.example.com",
		},
		{
			name:     "hostname shorthand and user",
			input:    "-h github.example.com -u monalisa",
			wantHost: "github.example.com",
			wantUser: "monalisa",
		},
		{
			name:     "json flag",
			input:    "--json hosts",
			wantJSON: true,
		},
		{
			name:       "unknown json field",
			input:      "--json tokens",
			wantErr:    true,
			wantErrMsg: "Unknown JSON field: \"tokens\"\nAvailable fields:\n  hosts",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ios, _, _, _ := iostreams.Test()
			f := &cmdutil.Factory{
				IOStreams: ios,
				Config: func() (gh.Config, error) {
					return config.NewMockConfig(), nil
				},
			}
			argv, err := shlex.Split(tt.input)
			require.NoError(t, err)

			var seen *resolveOptions
			cmd := NewCmdResolve(f, func(opts *resolveOptions) error {
				seen = opts
				return nil
			})
			cmd.Flags().BoolP("help", "x", false, "")
			cmd.SetArgs(argv)
			cmd.SetIn(&bytes.Buffer{})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})

			_, err = cmd.ExecuteC()
			if tt.wantErr {
				require.Error(t, err)
				require.EqualError(t, err, tt.wantErrMsg)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantHost, seen.Hostname)
			require.Equal(t, tt.wantUser, seen.Username)
			if tt.wantJSON {
				require.NotNil(t, seen.Exporter)
			} else {
				require.Nil(t, seen.Exporter)
			}
		})
	}
}

func TestResolveRunNoCredentials(t *testing.T) {
	ios, _, stdout, stderr := iostreams.Test()
	cfg, _ := config.NewIsolatedTestConfig(t, "")

	opts := newOpts(t, ios, cfg)
	require.NoError(t, resolveRun(opts))

	require.Equal(t, "No credentials are stored for any host known to gh.\n", stdout.String())
	require.Empty(t, stderr.String(), "no hint or diagnostic is printed")
}

func TestResolveRunUnknownHost(t *testing.T) {
	ios, _, _, _ := iostreams.Test()
	cfg, _ := config.NewIsolatedTestConfig(t, "")
	_, err := cfg.Authentication().Login("github.com", "monalisa", "ghp_token00000000000000000000000000000000", "https", false)
	require.NoError(t, err)

	opts := newOpts(t, ios, cfg)
	opts.Hostname = "unknown.example.com"
	err = resolveRun(opts)
	require.EqualError(t, err, `unknown host "unknown.example.com": gh has no credentials configured for it`)
}

func TestResolveRunUnknownAccount(t *testing.T) {
	cfg, _ := config.NewIsolatedTestConfig(t, "")
	_, err := cfg.Authentication().Login("github.com", "monalisa", "ghp_token00000000000000000000000000000000", "https", false)
	require.NoError(t, err)

	t.Run("on a given host", func(t *testing.T) {
		ios, _, _, _ := iostreams.Test()
		opts := newOpts(t, ios, cfg)
		opts.Hostname = "github.com"
		opts.Username = "hubot"
		err := resolveRun(opts)
		require.EqualError(t, err, `unknown account "hubot" on host "github.com"`)
	})

	t.Run("across all hosts", func(t *testing.T) {
		ios, _, _, _ := iostreams.Test()
		opts := newOpts(t, ios, cfg)
		opts.Username = "hubot"
		err := resolveRun(opts)
		require.EqualError(t, err, `unknown account "hubot": no known host stores credentials for it`)
	})
}

func TestResolveRunAllHostsSortedAndTokenFree(t *testing.T) {
	ios, _, stdout, stderr := iostreams.Test()
	cfg, _ := config.NewIsolatedTestConfig(t, "")
	githubToken := "ghp_githubtoken0000000000000000000000000000"
	enterpriseToken := "ghp_enterprisetoken000000000000000000000000"
	_, err := cfg.Authentication().Login("zzz.example.com", "enterprise-user", enterpriseToken, "https", false)
	require.NoError(t, err)
	_, err = cfg.Authentication().Login("github.com", "monalisa", githubToken, "https", false)
	require.NoError(t, err)

	opts := newOpts(t, ios, cfg)
	require.NoError(t, resolveRun(opts))

	out := stdout.String()
	require.Less(t, indexOf(out, "github.com"), indexOf(out, "zzz.example.com"), "hosts are sorted")
	require.Contains(t, out, "Active account: monalisa")
	require.Contains(t, out, "state: selected")
	require.Contains(t, out, "state: absent")
	require.Contains(t, out, "config file")
	require.Contains(t, out, "secure storage")
	require.Contains(t, out, `prefix "ghp_"`)
	require.Contains(t, out, "characters")
	require.NotContains(t, out, githubToken, "no token content in output")
	require.NotContains(t, out, enterpriseToken)
	require.Empty(t, stderr.String())
}

func TestResolveRunHostnameAndUserFilter(t *testing.T) {
	ios, _, stdout, _ := iostreams.Test()
	cfg, _ := config.NewIsolatedTestConfig(t, "")
	_, err := cfg.Authentication().Login("zzz.example.com", "enterprise-user", "ghp_enterprise00000000000000000000000000", "https", false)
	require.NoError(t, err)
	_, err = cfg.Authentication().Login("github.com", "monalisa", "ghp_monalisa0000000000000000000000000000", "https", false)
	require.NoError(t, err)
	_, err = cfg.Authentication().Login("github.com", "hubot", "ghp_hubot000000000000000000000000000000", "https", false)
	require.NoError(t, err)

	opts := newOpts(t, ios, cfg)
	opts.Hostname = "github.com"
	opts.Username = "hubot"
	require.NoError(t, resolveRun(opts))

	out := stdout.String()
	require.Contains(t, out, "github.com")
	require.NotContains(t, out, "zzz.example.com")
	require.Contains(t, out, "hubot")
	require.NotContains(t, out, "monalisa")
}

func TestResolveRunReportsConflict(t *testing.T) {
	ios, _, stdout, _ := iostreams.Test()
	cfg, _ := config.NewIsolatedTestConfig(t, "")
	authCfg := cfg.Authentication()
	oldToken := "gho_oldtoken000000000000000000000000000000"
	_, err := authCfg.Login("github.com", "monalisa", oldToken, "https", true)
	require.NoError(t, err)
	// A later refresh lands in plain text while the stale keyring entry remains,
	// leaving two different credentials for the same account.
	newToken := "ghp_newtoken000000000000000000000000000000"
	_, err = authCfg.Login("github.com", "monalisa", newToken, "https", false)
	require.NoError(t, err)

	opts := newOpts(t, ios, cfg)
	require.NoError(t, resolveRun(opts))

	out := stdout.String()
	require.Contains(t, out, "duplicate_stored_credentials")
	require.Contains(t, out, "active_credential_mismatch")
	require.NotContains(t, out, oldToken)
	require.NotContains(t, out, newToken)
}

func TestResolveRunJSON(t *testing.T) {
	ios, _, stdout, stderr := iostreams.Test()
	cfg, _ := config.NewIsolatedTestConfig(t, "")
	token := "ghp_jsontoken000000000000000000000000000000"
	_, err := cfg.Authentication().Login("github.com", "monalisa", token, "https", false)
	require.NoError(t, err)

	exporter := cmdutil.NewJSONExporter()
	exporter.SetFields([]string{"hosts"})
	opts := newOpts(t, ios, cfg)
	opts.Exporter = exporter
	require.NoError(t, resolveRun(opts))

	var report struct {
		Hosts []struct {
			Hostname     string `json:"hostname"`
			ActiveUser   string `json:"activeUser"`
			ActiveStep   int    `json:"activeStep"`
			ActiveSource string `json:"activeSource"`
			Sources      []struct {
				Step        int    `json:"step"`
				Class       string `json:"class"`
				State       string `json:"state"`
				Location    string `json:"location"`
				Identifier  string `json:"identifier"`
				Fingerprint struct {
					Length int    `json:"length"`
					Prefix string `json:"prefix"`
				} `json:"fingerprint"`
			} `json:"sources"`
			Accounts []struct {
				Login                    string `json:"login"`
				Active                   bool   `json:"active"`
				EffectiveStep            int    `json:"effectiveStep"`
				CommandsUseOwnCredential bool   `json:"commandsUseOwnCredential"`
			} `json:"accounts"`
		} `json:"hosts"`
	}
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &report))
	require.Len(t, report.Hosts, 1)
	host := report.Hosts[0]
	require.Equal(t, "github.com", host.Hostname)
	require.Equal(t, "monalisa", host.ActiveUser)
	require.Equal(t, 3, host.ActiveStep)
	require.Equal(t, "oauth_token", host.ActiveSource)
	require.Len(t, host.Sources, 5)
	require.Equal(t, "environment_variable", host.Sources[0].Class)
	require.Equal(t, "GH_TOKEN", host.Sources[0].Location)
	require.Equal(t, "config_file", host.Sources[2].Class)
	require.Equal(t, "selected", host.Sources[2].State)
	require.Equal(t, len(token), host.Sources[2].Fingerprint.Length)
	require.Equal(t, "ghp_", host.Sources[2].Fingerprint.Prefix)
	require.Len(t, host.Accounts, 1)
	require.True(t, host.Accounts[0].Active)
	require.Equal(t, 2, host.Accounts[0].EffectiveStep)
	require.True(t, host.Accounts[0].CommandsUseOwnCredential)
	require.NotContains(t, stdout.String(), token)
	require.Empty(t, stderr.String())
}

func TestResolveRunJSONEmptyResult(t *testing.T) {
	ios, _, stdout, _ := iostreams.Test()
	cfg, _ := config.NewIsolatedTestConfig(t, "")

	exporter := cmdutil.NewJSONExporter()
	exporter.SetFields([]string{"hosts"})
	opts := newOpts(t, ios, cfg)
	opts.Exporter = exporter
	require.NoError(t, resolveRun(opts))

	require.JSONEq(t, `{"hosts":[]}`, stdout.String())
}

func TestResolveRunIsReadOnly(t *testing.T) {
	ios, _, _, _ := iostreams.Test()
	cfg, readConfigs := config.NewIsolatedTestConfig(t, "")
	_, err := cfg.Authentication().Login("github.com", "monalisa", "ghp_readonly0000000000000000000000000000", "https", false)
	require.NoError(t, err)

	var beforeHosts, beforeConfig bytes.Buffer
	readConfigs(&beforeConfig, &beforeHosts)

	opts := newOpts(t, ios, cfg)
	require.NoError(t, resolveRun(opts))

	var afterHosts, afterConfig bytes.Buffer
	readConfigs(&afterConfig, &afterHosts)
	assert.Equal(t, beforeConfig.String(), afterConfig.String(), "config file untouched")
	assert.Equal(t, beforeHosts.String(), afterHosts.String(), "hosts file untouched")
}

func indexOf(haystack, needle string) int {
	idx := bytes.Index([]byte(haystack), []byte(needle))
	if idx < 0 {
		return len(haystack)
	}
	return idx
}
