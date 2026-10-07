package resolve

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cli/cli/v2/git"
	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/internal/run"
	"github.com/cli/cli/v2/internal/telemetry"
	"github.com/cli/cli/v2/pkg/cmd/factory"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/httpmock"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/google/shlex"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const githubCfg = `
version: 1
hosts:
    github.com:
        oauth_token: token123
        user: me
`

// stubGitCommands registers the read-only git commands resolveRun executes.
// GitClient.Remotes is reached twice (once raw, once via the factory) and
// RemoteResolutionRecords once, so each command is stubbed accordingly.
func stubGitCommands(cs *run.CommandStubber, remoteOut, configOut string, configExit int) {
	cs.Register(`git remote -v`, 0, remoteOut)
	cs.Register(`git remote -v`, 0, remoteOut)
	cs.Register(`git config --get-regexp`, configExit, configOut)
	cs.Register(`git config --get-regexp`, configExit, configOut)
	cs.Register(`git config --get-regexp`, configExit, configOut)
}

type captureExporter struct {
	data any
}

func (c *captureExporter) Fields() []string { return nil }
func (c *captureExporter) Write(_ *iostreams.IOStreams, data any) error {
	c.data = data
	return nil
}

func newTestFactory(t *testing.T, cfgString string, reg *httpmock.Registry) (*cmdutil.Factory, *iostreams.IOStreams) {
	t.Helper()
	cfg, _ := config.NewIsolatedTestConfig(t, cfgString)
	ios, _, _, _ := iostreams.Test()
	f := factory.New("v1.2.3", "", func() (gh.Config, error) { return cfg, nil }, ios, "git", &telemetry.NoOpService{})
	if reg != nil {
		f.HttpClient = func() (*http.Client, error) { return &http.Client{Transport: reg}, nil }
	}
	return f, ios
}

func runResolution(t *testing.T, f *cmdutil.Factory, remoteOut, configOut string, configExit int) *report {
	t.Helper()
	cs, teardown := run.Stub()
	defer teardown(t)
	stubGitCommands(cs, remoteOut, configOut, configExit)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO:         f.IOStreams,
		Config:     f.Config,
		HttpClient: f.HttpClient,
		GitClient:  f.GitClient,
		Remotes:    f.Remotes,
		Exporter:   capture,
	}
	require.NoError(t, resolveRun(opts))

	return capture.data.(*report)
}

func TestResolveRunSingleRemoteNonInteractive(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"repo","owner":{"login":"owner"},
			"viewerPermission":"READ","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null}}}`))

	f, _ := newTestFactory(t, githubCfg, reg)
	remoteOut := "origin\thttps://github.com/owner/repo.git (fetch)\norigin\thttps://github.com/owner/repo.git (push)\n"
	rep := runResolution(t, f, remoteOut, "", 1)

	assert.Equal(t, statusResolved, rep.Status)
	require.NotNil(t, rep.Selected)
	assert.Equal(t, "owner/repo", rep.Selected.NameWithOwner)
	assert.Equal(t, "github.com", rep.Selected.Host)
	assert.Equal(t, "origin", rep.SelectedRemote)
	assert.Equal(t, "non_interactive_fallback", rep.Rule)
	assert.True(t, rep.FallbackInEffect)

	require.Len(t, rep.Candidates, 1)
	c := rep.Candidates[0]
	assert.True(t, c.Selected)
	assert.True(t, c.Considered)
	assert.Equal(t, availabilityReturned, c.ApiAvailability)
	assert.Empty(t, c.Reasons)

	assert.True(t, rep.Interactive.Available)
	assert.Equal(t, "network_single", rep.Interactive.Rule)
	assert.True(t, rep.NonInteractive.Available)
	assert.Equal(t, "non_interactive_fallback", rep.NonInteractive.Rule)
	assert.Nil(t, rep.Failure)
}

func TestResolveRunAmbiguousInteractive(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"repo","owner":{"login":"owner"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null},
			"repo_001":{"id":"2","name":"repo","owner":{"login":"me"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,
			"parent":{"id":"1","name":"repo","owner":{"login":"owner"}}}}}`))

	f, ios := newTestFactory(t, githubCfg, reg)
	// Non-TTY primary: fallback to the first sorted remote (upstream), while
	// the interactive outcome is ambiguous.
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/me/repo.git (fetch)\norigin\thttps://github.com/me/repo.git (push)\n" +
		"upstream\thttps://github.com/owner/repo.git (fetch)\nupstream\thttps://github.com/owner/repo.git (push)\n"
	stubGitCommands(cs, remoteOut, "", 1)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: ios, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.NoError(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, statusResolved, rep.Status)
	assert.Equal(t, "owner/repo", rep.Selected.NameWithOwner)
	assert.Equal(t, "upstream", rep.SelectedRemote)
	assert.Equal(t, "non_interactive_fallback", rep.Rule)

	// Interactive outcome reports ambiguity.
	assert.Equal(t, "ambiguous", rep.Interactive.Rule)
	assert.Nil(t, rep.Interactive.Repository)
	assert.True(t, rep.NonInteractive.Available)

	// Both remotes are considered; no host filter narrowed the set.
	assert.Equal(t, scopeAll, rep.Scope.Scope)
	assert.Equal(t, 2, rep.Scope.TotalRemotes)
	assert.Equal(t, 2, rep.Scope.ConsideredRemotes)

	byRemote := map[string]candidateReport{}
	for _, c := range rep.Candidates {
		byRemote[c.Remote] = c
	}
	assert.True(t, byRemote["upstream"].Selected)
	assert.True(t, byRemote["origin"].Considered)
}

func TestResolveRunResolvedBaseRecordValid(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"repo","owner":{"login":"owner"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null}}}`))

	f, ios := newTestFactory(t, githubCfg, reg)
	ios.SetStdinTTY(true)
	ios.SetStdoutTTY(true)

	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/owner/repo.git (fetch)\norigin\thttps://github.com/owner/repo.git (push)\n"
	configOut := "remote.origin.gh-resolved base\n"
	stubGitCommands(cs, remoteOut, configOut, 0)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: ios, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.NoError(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, statusResolved, rep.Status)
	assert.Equal(t, "resolved_base", rep.Rule)
	assert.False(t, rep.FallbackInEffect)

	require.Len(t, rep.DefaultRecords, 1)
	record := rep.DefaultRecords[0]
	assert.Equal(t, "origin", record.Remote)
	assert.Equal(t, "base", record.Value)
	require.NotNil(t, record.Valid)
	assert.True(t, *record.Valid)
	assert.True(t, record.Effective)
	assert.Empty(t, record.Issues)
}

func TestResolveRunRecordTargetRenamed(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"newname","owner":{"login":"owner"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null}}}`))

	f, _ := newTestFactory(t, githubCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/owner/oldname.git (fetch)\norigin\thttps://github.com/owner/oldname.git (push)\n"
	configOut := "remote.origin.gh-resolved base\n"
	stubGitCommands(cs, remoteOut, configOut, 0)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: f.IOStreams, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.NoError(t, resolveRun(opts))
	rep := capture.data.(*report)

	// The chain trusts the record, so the selection keeps the local old name.
	assert.Equal(t, "owner/oldname", rep.Selected.NameWithOwner)
	assert.Equal(t, "resolved_base", rep.Rule)

	require.Len(t, rep.DefaultRecords, 1)
	record := rep.DefaultRecords[0]
	require.NotNil(t, record.Valid)
	assert.False(t, *record.Valid)
	assert.Contains(t, record.Issues, "repository_renamed")

	// The API name appears as a candidate and differs from the selection.
	var apiCand *candidateReport
	for i := range rep.Candidates {
		if rep.Candidates[i].NameWithOwner == "owner/newname" {
			apiCand = &rep.Candidates[i]
		}
	}
	require.NotNil(t, apiCand)
	assert.Equal(t, availabilityReturned, apiCand.ApiAvailability)
}

func TestResolveRunRecordPointsToMissingRepository(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},"repo_000":null}}`))

	f, _ := newTestFactory(t, githubCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/owner/repo.git (fetch)\norigin\thttps://github.com/owner/repo.git (push)\n"
	configOut := "remote.origin.gh-resolved owner/gone\n"
	stubGitCommands(cs, remoteOut, configOut, 0)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: f.IOStreams, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.NoError(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, "owner/gone", rep.Selected.NameWithOwner)
	assert.Equal(t, "resolved_repository", rep.Rule)

	require.Len(t, rep.DefaultRecords, 1)
	record := rep.DefaultRecords[0]
	require.NotNil(t, record.Valid)
	assert.False(t, *record.Valid)
	assert.Contains(t, record.Issues, "repository_not_found")

	var originCand, goneCand *candidateReport
	for i := range rep.Candidates {
		switch rep.Candidates[i].NameWithOwner {
		case "owner/repo":
			originCand = &rep.Candidates[i]
		case "owner/gone":
			goneCand = &rep.Candidates[i]
		}
	}
	require.NotNil(t, originCand)
	require.NotNil(t, goneCand)
	assert.Equal(t, availabilityNotFound, originCand.ApiAvailability)
	assert.True(t, goneCand.Selected)
}

func TestResolveRunExplicitFlagOverridesRecord(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"repo","owner":{"login":"owner"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null}}}`))

	f, _ := newTestFactory(t, githubCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/owner/repo.git (fetch)\norigin\thttps://github.com/owner/repo.git (push)\n"
	configOut := "remote.origin.gh-resolved base\n"
	stubGitCommands(cs, remoteOut, configOut, 0)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: f.IOStreams, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
		RepoOverride: "other/repo",
	}
	require.NoError(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, "other/repo", rep.Selected.NameWithOwner)
	assert.Equal(t, "explicit_override", rep.Rule)
	assert.True(t, rep.Override.Present)
	assert.Equal(t, "flag", rep.Override.Source)
	require.NotNil(t, rep.Override.Valid)
	assert.True(t, *rep.Override.Valid)

	require.Len(t, rep.DefaultRecords, 1)
	record := rep.DefaultRecords[0]
	assert.False(t, record.Effective)
	assert.Contains(t, record.Issues, "not_effective_explicit_override")
	assert.Contains(t, record.Issues, "target_differs_from_resolution_result")
}

func TestResolveRunEnvironmentOverride(t *testing.T) {
	t.Setenv("GH_REPO", "somehost.com/other/repo")

	enterpriseCfg := `
version: 1
hosts:
    github.com:
        oauth_token: token123
        user: me
    somehost.com:
        oauth_token: token456
        user: me
`
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"repo","owner":{"login":"other"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null}}}`))

	f, _ := newTestFactory(t, enterpriseCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://somehost.com/other/repo.git (fetch)\norigin\thttps://somehost.com/other/repo.git (push)\n"
	stubGitCommands(cs, remoteOut, "", 1)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: f.IOStreams, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.NoError(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, "other/repo", rep.Selected.NameWithOwner)
	assert.Equal(t, "somehost.com", rep.Selected.Host)
	assert.Equal(t, "environment", rep.Override.Source)
	assert.Equal(t, "explicit_override", rep.Rule)
}

func TestResolveRunInvalidOverride(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	f, _ := newTestFactory(t, githubCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/owner/repo.git (fetch)\norigin\thttps://github.com/owner/repo.git (push)\n"
	stubGitCommands(cs, remoteOut, "", 1)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: f.IOStreams, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
		RepoOverride: "not_a_valid_value",
	}
	require.Error(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, statusFailed, rep.Status)
	require.NotNil(t, rep.Failure)
	assert.Equal(t, "explicit_override", rep.Failure.Step)
	require.NotNil(t, rep.Override.Valid)
	assert.False(t, *rep.Override.Valid)
	// Candidates are still reported despite the failure.
	assert.NotEmpty(t, rep.Candidates)
}

func TestResolveRunEmptyRemotes(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	f, _ := newTestFactory(t, githubCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	stubGitCommands(cs, "", "", 1)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: f.IOStreams, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.Error(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, statusFailed, rep.Status)
	assert.Empty(t, rep.Candidates)
	require.NotNil(t, rep.Failure)
	assert.Equal(t, "remote_selection", rep.Failure.Step)
	assert.False(t, rep.Interactive.Available)
	assert.False(t, rep.NonInteractive.Available)
}

func TestResolveRunNetworkUnreachableInteractive(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StatusStringResponse(500, "internal server error"))

	f, ios := newTestFactory(t, githubCfg, reg)
	ios.SetStdinTTY(true)
	ios.SetStdoutTTY(true)

	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/owner/repo.git (fetch)\norigin\thttps://github.com/owner/repo.git (push)\n"
	stubGitCommands(cs, remoteOut, "", 1)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: ios, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.Error(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, statusFailed, rep.Status)
	require.NotNil(t, rep.Failure)
	assert.Equal(t, "network_resolution", rep.Failure.Step)
	assert.NotEmpty(t, rep.Interactive.Error)

	// The non-interactive path still has a fallback result.
	require.NotNil(t, rep.NonInteractive.Repository)
	assert.Equal(t, "non_interactive_fallback", rep.NonInteractive.Rule)

	for _, c := range rep.Candidates {
		assert.Equal(t, availabilityUnreachable, c.ApiAvailability)
	}
}

func TestResolveRunHostNotAuthenticated(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	f, _ := newTestFactory(t, githubCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://example.org/owner/repo.git (fetch)\norigin\thttps://example.org/owner/repo.git (push)\n"
	stubGitCommands(cs, remoteOut, "", 1)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: f.IOStreams, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.Error(t, resolveRun(opts))
	rep := capture.data.(*report)

	assert.Equal(t, statusFailed, rep.Status)
	require.NotNil(t, rep.Failure)
	assert.Equal(t, "remote_selection", rep.Failure.Step)

	require.Len(t, rep.Candidates, 1)
	c := rep.Candidates[0]
	assert.False(t, c.Considered)
	assert.Contains(t, c.Reasons, "host_not_authenticated")
}

func TestResolveRunStaleRecordRemoteDeleted(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"repo","owner":{"login":"owner"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null}}}`))

	f, _ := newTestFactory(t, githubCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/owner/repo.git (fetch)\norigin\thttps://github.com/owner/repo.git (push)\n"
	configOut := "remote.oldorigin.gh-resolved base\n"
	stubGitCommands(cs, remoteOut, configOut, 0)

	capture := &captureExporter{}
	opts := &ResolveOptions{
		IO: f.IOStreams, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes, Exporter: capture,
	}
	require.NoError(t, resolveRun(opts))
	rep := capture.data.(*report)

	var stale *candidateReport
	for i := range rep.Candidates {
		if rep.Candidates[i].Remote == "oldorigin" {
			stale = &rep.Candidates[i]
		}
	}
	require.NotNil(t, stale)
	assert.False(t, stale.RemoteExists)
	assert.False(t, stale.Considered)
	assert.Equal(t, "base", stale.ResolutionValue)
	assert.Contains(t, stale.Reasons, "remote_no_longer_exists")

	require.Len(t, rep.DefaultRecords, 1)
	record := rep.DefaultRecords[0]
	assert.Equal(t, "oldorigin", record.Remote)
	require.NotNil(t, record.Valid)
	assert.False(t, *record.Valid)
	assert.False(t, record.Effective)
}

func TestResolveRunHumanOutputAndExit(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"repo","owner":{"login":"owner"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null}}}`))

	f, ios := newTestFactory(t, githubCfg, reg)
	cs, teardown := run.Stub()
	defer teardown(t)
	remoteOut := "origin\thttps://github.com/owner/repo.git (fetch)\norigin\thttps://github.com/owner/repo.git (push)\n"
	configOut := "remote.origin.gh-resolved base\n"
	stubGitCommands(cs, remoteOut, configOut, 0)

	opts := &ResolveOptions{
		IO: ios, Config: f.Config, HttpClient: f.HttpClient,
		GitClient: f.GitClient, Remotes: f.Remotes,
	}
	require.NoError(t, resolveRun(opts))
}

func TestResolveCommandJSONOutput(t *testing.T) {
	reg := &httpmock.Registry{}
	defer reg.Verify(t)
	reg.Register(
		httpmock.GraphQL("RepositoryNetwork"),
		httpmock.StringResponse(`
			{"data":{"viewer":{"login":"me"},
			"repo_000":{"id":"1","name":"repo","owner":{"login":"owner"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,"parent":null},
			"repo_001":{"id":"2","name":"repo","owner":{"login":"me"},
			"viewerPermission":"ADMIN","defaultBranchRef":{"name":"main"},
			"isPrivate":false,
			"parent":{"id":"1","name":"repo","owner":{"login":"owner"}}}}}`))

	cfg, _ := config.NewIsolatedTestConfig(t, githubCfg)
	ios, _, stdout, stderr := iostreams.Test()
	// Interactive primary: the disambiguation rule fails, so the report is
	// emitted with a non-zero exit. Set TTYs explicitly.
	ios.SetStdinTTY(true)
	ios.SetStdoutTTY(true)
	f := factory.New("v1.2.3", "", func() (gh.Config, error) { return cfg, nil }, ios, "git", &telemetry.NoOpService{})
	f.HttpClient = func() (*http.Client, error) { return &http.Client{Transport: reg}, nil }

	cmd := NewCmdResolve(f, nil)
	cs, teardown := run.Stub()
	defer teardown(t)
	cs.Register(`git rev-parse --git-dir`, 0, ".git")
	remoteOut := "origin\thttps://github.com/me/repo.git (fetch)\norigin\thttps://github.com/me/repo.git (push)\n" +
		"upstream\thttps://github.com/owner/repo.git (fetch)\nupstream\thttps://github.com/owner/repo.git (push)\n"
	stubGitCommands(cs, remoteOut, "", 1)

	argv, err := shlex.Split("--format json")
	require.NoError(t, err)
	cmd.SetArgs(argv)

	_, execErr := cmd.ExecuteC()
	require.Error(t, execErr)

	// Machine-readable output: JSON on stdout, no prompt lines anywhere.
	assert.Empty(t, stderr.String())
	var rep report
	require.NoError(t, json.Unmarshal(stdout.Bytes(), &rep))
	assert.Equal(t, statusFailed, rep.Status)
	require.NotNil(t, rep.Failure)
	assert.Equal(t, "disambiguation", rep.Failure.Step)
	assert.Equal(t, scopeAll, rep.Scope.Scope)
}

func TestNewCmdResolve(t *testing.T) {
	tests := []struct {
		name         string
		argv         string
		gitStubs     func(*run.CommandStubber)
		wantErr      string
		wantOverride string
	}{
		{
			name: "no args inside repository",
			argv: "",
			gitStubs: func(cs *run.CommandStubber) {
				cs.Register(`git rev-parse --git-dir`, 0, ".git")
			},
		},
		{
			name: "repo flag",
			argv: "--repo owner/repo",
			gitStubs: func(cs *run.CommandStubber) {
				cs.Register(`git rev-parse --git-dir`, 0, ".git")
			},
			wantOverride: "owner/repo",
		},
		{
			name:     "positional argument rejected",
			argv:     "owner/repo",
			gitStubs: func(cs *run.CommandStubber) {},
			wantErr:  `unknown command "owner/repo" for "resolve"`,
		},
		{
			name: "outside repository",
			argv: "",
			gitStubs: func(cs *run.CommandStubber) {
				cs.Register(`git rev-parse --git-dir`, 128, ".git")
			},
			wantErr: "must be run from inside a git repository",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ios, _, _, _ := iostreams.Test()
			var gotOpts *ResolveOptions
			f := &cmdutil.Factory{IOStreams: ios, GitClient: &git.Client{}}
			cmd := NewCmdResolve(f, func(o *ResolveOptions) error {
				gotOpts = o
				return nil
			})

			cs, teardown := run.Stub()
			defer teardown(t)
			tt.gitStubs(cs)

			argv, err := shlex.Split(tt.argv)
			require.NoError(t, err)
			cmd.SetArgs(argv)

			_, err = cmd.ExecuteC()
			if tt.wantErr != "" {
				require.Error(t, err)
				assert.Equal(t, tt.wantErr, err.Error())
				return
			}
			require.NoError(t, err)
			require.NotNil(t, gotOpts)
			assert.Equal(t, tt.wantOverride, gotOpts.RepoOverride)
		})
	}
}
