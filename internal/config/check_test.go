package config

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cli/cli/v2/internal/gh"
	ghmock "github.com/cli/cli/v2/internal/gh/mock"
	ghConfig "github.com/cli/go-gh/v2/pkg/config"
	"github.com/stretchr/testify/require"
)

const cleanConfigYAML = `
version: 1
git_protocol: https
prompt: enabled
prefer_editor_prompt: disabled
clipboard: enabled
color_labels: disabled
accessible_colors: disabled
accessible_prompter: disabled
spinner: enabled
telemetry: enabled
http_unix_socket:
browser:
editor:
pager:
aliases:
  co: pr checkout
hosts:
  github.com:
    user: mona
    git_protocol: https
    api_host: github.com
    users:
      mona:
        oauth_token: TOKEN
`

func TestCheckCleanConfig(t *testing.T) {
	issues, err := checkFromYAML(cleanConfigYAML)
	require.NoError(t, err)
	require.Empty(t, issues)
}

func TestCheckIssues(t *testing.T) {
	tests := []struct {
		name        string
		cfgYAML     string
		wantIssue   Issue
		wantMessage string
	}{
		{
			name: "unknown global key",
			cfgYAML: `
frobnicate: yes
`,
			wantIssue: Issue{
				Kind:          IssueUnknownKey,
				Location:      "global",
				Key:           "frobnicate",
				CurrentValue:  "yes",
				ExpectedValue: "a known configuration key (run 'gh config' to list supported keys)",
				Source:        gh.ConfigUserProvided,
			},
			wantMessage: `global: unknown key "frobnicate" with value "yes"; expected a known configuration key (run 'gh config' to list supported keys); value from user configuration`,
		},
		{
			name: "invalid global value",
			cfgYAML: `
git_protocol: svn
`,
			wantIssue: Issue{
				Kind:          IssueInvalidValue,
				Location:      "global",
				Key:           gitProtocolKey,
				CurrentValue:  "svn",
				ExpectedValue: "one of https, ssh",
				Source:        gh.ConfigUserProvided,
			},
			wantMessage: `global: key "git_protocol" has invalid value "svn"; expected one of https, ssh; value from user configuration`,
		},
		{
			name: "host only key set globally",
			cfgYAML: `
api_host: gateway.example.com
`,
			wantIssue: Issue{
				Kind:          IssueHostOnlyInGlobal,
				Location:      "global",
				Key:           apiHostKey,
				CurrentValue:  "gateway.example.com",
				ExpectedValue: "the key to be set for a host with --host",
				Source:        gh.ConfigUserProvided,
			},
			wantMessage: `global: key "api_host" requires a host but is set globally; current value "gateway.example.com"; expected the key to be set for a host with --host; value from user configuration`,
		},
		{
			name: "global only key set for a host",
			cfgYAML: `
hosts:
  github.com:
    clipboard: enabled
`,
			wantIssue: Issue{
				Kind:          IssueGlobalOnlyInHost,
				Location:      "host github.com",
				Key:           clipboardKey,
				CurrentValue:  "enabled",
				ExpectedValue: "the key to be unset for this host; it can only be set globally",
				Source:        gh.ConfigUserProvided,
			},
			wantMessage: `host github.com: key "clipboard" can only be set globally but is set for the host; current value "enabled"; expected the key to be unset for this host; it can only be set globally; value from user configuration`,
		},
		{
			name: "unknown key under a host",
			cfgYAML: `
hosts:
  example.com:
    mystery: value
`,
			wantIssue: Issue{
				Kind:          IssueUnknownKey,
				Location:      "host example.com",
				Key:           "mystery",
				CurrentValue:  "value",
				ExpectedValue: "a known configuration key (run 'gh config' to list supported keys)",
				Source:        gh.ConfigUserProvided,
			},
		},
		{
			name: "unknown key under a user",
			cfgYAML: `
hosts:
  example.com:
    users:
      mona:
        mystery: value
`,
			wantIssue: Issue{
				Kind:          IssueUnknownKey,
				Location:      "host example.com",
				Key:           "mystery",
				CurrentValue:  "value",
				ExpectedValue: "a known configuration key (run 'gh config' to list supported keys)",
				Source:        gh.ConfigUserProvided,
			},
		},
		{
			name: "self referential alias",
			cfgYAML: `
aliases:
  repo x: repo x
`,
			wantIssue: Issue{
				Kind:          IssueAliasSelfReference,
				Location:      "alias repo x",
				Key:           "repo x",
				CurrentValue:  "repo x",
				ExpectedValue: `an expansion that does not return to "repo x"`,
				Source:        gh.ConfigUserProvided,
				Chain:         []string{"repo x", "repo x"},
			},
			wantMessage: `alias "repo x" is self-referential: expansion "repo x" returns to the alias; expected an expansion that does not return to "repo x"; value from user configuration`,
		},
		{
			name: "circular alias",
			cfgYAML: `
aliases:
  a: b
  b: a
`,
			wantIssue: Issue{
				Kind:          IssueAliasCircular,
				Location:      "alias a",
				Key:           "a",
				CurrentValue:  "b",
				ExpectedValue: "the chain to end at a gh command",
				Source:        gh.ConfigUserProvided,
				Chain:         []string{"a", "b", "a"},
			},
			wantMessage: `alias "a" is in a circular reference chain a -> b -> a; expected the chain to end at a gh command; value from user configuration`,
		},
		{
			name:    "over deep alias",
			cfgYAML: overDeepConfigYAML,
			wantIssue: Issue{
				Kind:          IssueAliasDepth,
				Location:      "alias a00",
				Key:           "a00",
				CurrentValue:  "a01",
				ExpectedValue: "an expansion depth of at most 10",
				Source:        gh.ConfigUserProvided,
				Chain:         chainNames(MaxAliasExpansionDepth + 1),
				Depth:         MaxAliasExpansionDepth + 1,
			},
			wantMessage: `alias "a00" expansion depth 11 exceeds the maximum depth 10; expected an expansion depth of at most 10; value from user configuration`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			issues, err := checkFromYAML(tt.cfgYAML)
			require.NoError(t, err)

			var matched *Issue
			for i := range issues {
				if issues[i].Key == tt.wantIssue.Key && issues[i].Location == tt.wantIssue.Location {
					matched = &issues[i]
					break
				}
			}
			require.NotNil(t, matched, "missing issue %q in %q", tt.wantIssue.Key, tt.wantIssue.Location)
			require.Equal(t, tt.wantIssue, *matched)

			if tt.wantMessage != "" {
				require.Equal(t, tt.wantMessage, FormatIssue(*matched))
			}
		})
	}
}

func TestCheckOrdering(t *testing.T) {
	cfgYAML := `
git_protocol: svn
zzz: true
hosts:
  github.com:
    clipboard: enabled
aliases:
  x: x
`
	issues, err := checkFromYAML(cfgYAML)
	require.NoError(t, err)

	order := []string{
		"global/" + gitProtocolKey,
		"global/zzz",
		"host github.com/" + clipboardKey,
		"alias x/x",
	}
	gotOrder := make([]string, len(issues))
	for i, issue := range issues {
		gotOrder[i] = issue.Location + "/" + issue.Key
	}
	require.Equal(t, order, gotOrder)
}

func TestCheckMixedProblemsDoNotBlockOtherFindings(t *testing.T) {
	cfgYAML := `
git_protocol: https
prompt: enabled
zzz: true
hosts:
  github.com:
    clipboard: enabled
    git_protocol: https
aliases:
  good: pr status
  bad: bad
`
	issues, err := checkFromYAML(cfgYAML)
	require.NoError(t, err)
	require.Len(t, issues, 3)
}

func TestCheckUnsupportedConfig(t *testing.T) {
	_, err := Check(&ghmock.ConfigMock{})
	require.EqualError(t, err, "configuration does not support health checks")
}

func checkFromYAML(cfgYAML string) ([]Issue, error) {
	c := &cfg{cfg: ghConfig.ReadFromString(cfgYAML)}
	return Check(c)
}

var overDeepConfigYAML = buildChainConfigYAML(MaxAliasExpansionDepth + 1)

// buildChainConfigYAML renders chainAliases as an aliases-only config YAML.
func buildChainConfigYAML(hoppings int) string {
	chains := chainAliases(hoppings)
	var sb strings.Builder
	sb.WriteString("aliases:\n")
	for _, name := range chainNames(hoppings) {
		sb.WriteString(fmt.Sprintf("  %s: %s\n", name, chains[name]))
	}
	return sb.String()
}
