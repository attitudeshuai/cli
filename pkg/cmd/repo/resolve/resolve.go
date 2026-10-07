// Package resolve implements the read-only `gh repo resolve` command, which
// explains how the base repository for the current directory is selected.
package resolve

import (
	ctx "context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/context"
	"github.com/cli/cli/v2/git"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/cli/go-gh/v2/pkg/ssh"
	"github.com/spf13/cobra"
)

type ResolveOptions struct {
	IO         *iostreams.IOStreams
	Config     func() (gh.Config, error)
	HttpClient func() (*http.Client, error)
	GitClient  *git.Client
	Remotes    func() (context.Remotes, error)
	Exporter   cmdutil.Exporter

	// RepoOverride is the --repo flag value, when present.
	RepoOverride string
}

// NewCmdResolve creates the `gh repo resolve` command.
func NewCmdResolve(f *cmdutil.Factory, runF func(*ResolveOptions) error) *cobra.Command {
	opts := &ResolveOptions{
		IO:         f.IOStreams,
		Config:     f.Config,
		HttpClient: f.HttpClient,
		GitClient:  f.GitClient,
		Remotes:    f.Remotes,
	}

	cmd := &cobra.Command{
		Use:   "resolve",
		Short: "Show how the base repository for this directory is resolved",
		Long: heredoc.Doc(`
			Resolve the base repository for the current working directory and report
			why it was selected, which other candidates were considered, and why
			those candidates did not take effect.

			This command is read-only: it does not modify git remote configuration,
			add or clear default repository records, or change the active account
			or token. The resolution uses the same chain as other gh commands.
		`),
		Example: heredoc.Doc(`
			# Show the base repository and the selection basis
			$ gh repo resolve

			# Emit a machine-readable report, including on resolution failure
			$ gh repo resolve --format json

			# Show how an explicit repository changes the resolution
			$ gh repo resolve --repo cli/cli
		`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if isLocal, err := opts.GitClient.IsLocalGitRepo(cmd.Context()); err != nil {
				return err
			} else if !isLocal {
				return errors.New("must be run from inside a git repository")
			}

			opts.RepoOverride, _ = cmd.Flags().GetString("repo")

			if runF != nil {
				return runF(opts)
			}
			return resolveRun(opts)
		},
	}

	cmdutil.EnableRepoOverride(cmd, f)
	cmdutil.AddFormatFlags(cmd, &opts.Exporter)

	return cmd
}

// repositoryRef is the stable, machine-readable reference to a repository.
type repositoryRef struct {
	NameWithOwner string `json:"nameWithOwner"`
	Host          string `json:"host"`
}

func newRepositoryRef(r ghrepo.Interface) *repositoryRef {
	return &repositoryRef{
		NameWithOwner: ghrepo.FullName(r),
		Host:          r.RepoHost(),
	}
}

type overrideReport struct {
	Present    bool           `json:"present"`
	Source     string         `json:"source"`
	Value      string         `json:"value"`
	Valid      *bool          `json:"valid"`
	Repository *repositoryRef `json:"repository"`
}

type hostsReport struct {
	Authenticated []string `json:"authenticated"`
	DefaultHost   string   `json:"defaultHost"`
	DefaultSource string   `json:"defaultSource"`
}

type scopeReport struct {
	Scope             string `json:"scope"`
	TotalRemotes      int    `json:"totalRemotes"`
	ConsideredRemotes int    `json:"consideredRemotes"`
}

type candidateReport struct {
	NameWithOwner    string   `json:"nameWithOwner"`
	Host             string   `json:"host"`
	Remote           string   `json:"remote"`
	RemoteExists     bool     `json:"remoteExists"`
	ResolutionValue  string   `json:"resolutionValue"`
	Considered       bool     `json:"considered"`
	ApiAvailability  string   `json:"apiAvailability"`
	ApiNameWithOwner string   `json:"apiNameWithOwner"`
	Selected         bool     `json:"selected"`
	Reasons          []string `json:"reasons"`
}

type outcomeReport struct {
	Available  bool           `json:"available"`
	Rule       string         `json:"rule"`
	Repository *repositoryRef `json:"repository"`
	Error      string         `json:"error"`
}

type defaultRecordReport struct {
	Remote       string         `json:"remote"`
	RemoteExists bool           `json:"remoteExists"`
	Value        string         `json:"value"`
	Target       *repositoryRef `json:"target"`
	Effective    bool           `json:"effective"`
	Valid        *bool          `json:"valid"`
	Issues       []string       `json:"issues"`
}

type failureReport struct {
	Step    string `json:"step"`
	Message string `json:"message"`
}

// report is the machine-readable resolution document. Field names are stable.
type report struct {
	CWD              string                `json:"cwd"`
	Status           string                `json:"status"`
	Selected         *repositoryRef        `json:"selected"`
	SelectedRemote   string                `json:"selectedRemote"`
	Rule             string                `json:"rule"`
	Override         overrideReport        `json:"override"`
	Hosts            hostsReport           `json:"hosts"`
	Scope            scopeReport           `json:"scope"`
	DefaultRecords   []defaultRecordReport `json:"defaultRecords"`
	Candidates       []candidateReport     `json:"candidates"`
	Interactive      outcomeReport         `json:"interactive"`
	NonInteractive   outcomeReport         `json:"nonInteractive"`
	FallbackInEffect bool                  `json:"fallbackInEffect"`
	Failure          *failureReport        `json:"failure"`
}

const (
	statusResolved = "resolved"
	statusFailed   = "failed"

	availabilityReturned    = "returned"
	availabilityNotFound    = "not_found"
	availabilityUnreachable = "unreachable"
	availabilityNotQueried  = "not_queried"

	scopeAll          = "all_remotes"
	scopeHostFiltered = "host_filtered_subset"
)

// rawEntry pairs a git remote with the repository it translates to, when any.
type rawEntry struct {
	gitRemote *git.Remote
	repo      ghrepo.Interface
}

func resolveRun(opts *ResolveOptions) error {
	cwd, _ := os.Getwd()
	rep := &report{
		CWD:            cwd,
		Status:         statusFailed,
		DefaultRecords: []defaultRecordReport{},
		Candidates:     []candidateReport{},
	}

	// The smart resolution chain obtains the HTTP client first.
	httpClient, httpClientErr := opts.HttpClient()
	var apiClient *api.Client
	if httpClientErr == nil {
		apiClient = api.NewClientFromHTTP(httpClient)
	}

	// Host and authentication information.
	cfg, cfgErr := opts.Config()
	var authedHosts []string
	defaultHost, defaultHostSrc := "", ""
	if cfgErr == nil {
		authCfg := cfg.Authentication()
		authedHosts = authCfg.Hosts()
		defaultHost, defaultHostSrc = authCfg.DefaultHost()
	}
	rep.Hosts = hostsReport{
		Authenticated: nonNilStrings(authedHosts),
		DefaultHost:   defaultHost,
		DefaultSource: defaultHostSrc,
	}

	// Explicit override resolution, mirroring OverrideBaseRepoFunc precedence:
	// the --repo flag wins, then the GH_REPO environment variable.
	override := opts.RepoOverride
	overrideSource := ""
	if override != "" {
		overrideSource = "flag"
	} else if envValue := os.Getenv("GH_REPO"); envValue != "" {
		override = envValue
		overrideSource = "environment"
	}
	rep.Override = buildOverrideReport(override, overrideSource)

	// Raw git remotes and all gh-resolved records, including stale ones.
	rawRemotes, rawErr := opts.GitClient.Remotes(ctx.Background())
	records, recordsErr := opts.GitClient.RemoteResolutionRecords(ctx.Background())

	translator := ssh.NewTranslator()
	raws := translateRawRemotes(rawRemotes, translator)
	rawByName := map[string]rawEntry{}
	for _, e := range raws {
		rawByName[e.gitRemote.Name] = e
	}

	// The filtered remotes are the actual input to the resolution chain.
	filtered, filteredErr := opts.Remotes()

	var resolved *context.ResolvedRemotes
	var r2rErr error
	if httpClientErr == nil && filteredErr == nil {
		resolved, r2rErr = context.ResolveRemotesToRepos(filtered, apiClient, override)
		if r2rErr != nil {
			resolved = nil
		}
	}

	// Evaluate the non-interactive outcome first since it never queries the
	// network, then the interactive one, which may.
	var interactive, nonInteractive context.Outcome
	if resolved != nil {
		nonInteractive = resolved.Resolve(false)
		interactive = resolved.Resolve(true)
	}

	// Fetch the raw network result for candidate reporting and record
	// validation. When an override short-circuits the rules, the chain itself
	// never fetches it, so this is a best-effort diagnostic fetch.
	var network *api.RepoNetworkResult
	var networkErr error
	if resolved != nil {
		network, networkErr = resolved.NetworkResult(context.DefaultRemotesForLookup)
	}
	if override != "" && networkErr != nil {
		// Selection does not depend on the network under an override, so an
		// unreachable API only prevents record validation, not resolution.
		networkErr = nil
		network = nil
	}

	primary := nonInteractive
	canPrompt := opts.IO.CanPrompt()
	if canPrompt {
		primary = interactive
	}

	// Assemble candidates from raw remotes, network-only repositories, and
	// stale gh-resolved records.
	candidates := buildCandidates(
		raws, filtered, records, recordsErr == nil, rep.Hosts.Authenticated, network, networkErr,
	)

	rep.Candidates = candidates
	rep.Scope = scopeReport{
		Scope:             candidateScope(raws, filtered),
		TotalRemotes:      len(raws),
		ConsideredRemotes: len(filtered),
	}

	if primary.Repo != nil {
		rep.Status = statusResolved
		rep.Selected = newRepositoryRef(primary.Repo)
		rep.Rule = string(primary.Rule)
		rep.FallbackInEffect = primary.Rule == context.RuleNonInteractiveFallback

		selected := findOrCreateSelectedCandidate(&rep.Candidates, primary.Repo)
		selected.Selected = true
		rep.SelectedRemote = selected.Remote
	}

	applyCandidateReasons(&rep.Candidates, primary, override != "")
	rep.DefaultRecords = buildDefaultRecords(
		records, recordsErr == nil, rawByName, filtered, network, networkErr != nil, override != "", primary,
	)
	rep.Interactive = buildOutcomeReport(interactive, resolved)
	rep.NonInteractive = buildOutcomeReport(nonInteractive, resolved)
	rep.Failure = determineFailure(
		cfgErr, httpClientErr, filteredErr, rawErr, r2rErr, networkErr, primary, canPrompt,
	)

	if opts.Exporter != nil {
		if err := opts.Exporter.Write(opts.IO, rep); err != nil {
			return err
		}
	} else {
		printHumanReport(opts.IO, rep)
	}

	if rep.Status == statusFailed {
		// The report already explains the failure on stdout; exit non-zero
		// without an additional error line.
		return cmdutil.SilentError
	}
	return nil
}

func buildOverrideReport(value, source string) overrideReport {
	out := overrideReport{
		Present: value != "",
		Source:  source,
		Value:   value,
	}
	if value != "" {
		repo, err := ghrepo.FromFullName(value)
		valid := err == nil
		out.Valid = &valid
		if valid {
			out.Repository = newRepositoryRef(repo)
		}
	}
	return out
}

func translateRawRemotes(rawRemotes git.RemoteSet, translator context.Translator) []rawEntry {
	entries := make([]rawEntry, 0, len(rawRemotes))
	for _, gr := range rawRemotes {
		var repo ghrepo.Interface
		if gr.FetchURL != nil {
			repo, _ = ghrepo.FromURL(translator.Translate(gr.FetchURL))
		}
		if gr.PushURL != nil && repo == nil {
			repo, _ = ghrepo.FromURL(translator.Translate(gr.PushURL))
		}
		entries = append(entries, rawEntry{gitRemote: gr, repo: repo})
	}
	return entries
}

// buildCandidates creates one entry per git remote, then adds repositories the
// API returned without a backing remote (e.g. fork parents), then stale records.
func buildCandidates(
	raws []rawEntry,
	filtered context.Remotes,
	records map[string]string,
	recordsOK bool,
	authedHosts []string,
	network *api.RepoNetworkResult,
	networkErr error,
) []candidateReport {
	considered := map[string]bool{}
	for _, rem := range filtered {
		considered[rem.Name] = true
	}

	candByRemote := map[string]*candidateReport{}
	out := make([]candidateReport, 0, len(raws))

	for _, e := range raws {
		c := candidateReport{
			Remote:          e.gitRemote.Name,
			RemoteExists:    true,
			ResolutionValue: e.gitRemote.Resolved,
			Considered:      considered[e.gitRemote.Name],
			ApiAvailability: availabilityNotQueried,
		}
		if e.repo != nil {
			c.NameWithOwner = ghrepo.FullName(e.repo)
			c.Host = e.repo.RepoHost()
		} else {
			c.Reasons = append(c.Reasons, "not_a_github_repository")
		}
		if !c.Considered && c.NameWithOwner != "" {
			c.Reasons = append(c.Reasons, exclusionReason(c.Host, authedHosts))
		}
		out = append(out, c)
	}

	for i := range out {
		candByRemote[out[i].Remote] = &out[i]
	}

	// Map network slots positionally onto the filtered remotes.
	slotRepos := map[string]*api.Repository{}
	if network != nil && networkErr == nil {
		for i, rem := range filtered {
			if i >= len(network.Repositories) {
				break
			}
			slotRepos[rem.Name] = network.Repositories[i]
		}
	}
	for name, c := range candByRemote {
		if !c.Considered {
			continue
		}
		switch {
		case networkErr != nil:
			c.ApiAvailability = availabilityUnreachable
		case network == nil:
			c.ApiAvailability = availabilityNotQueried
		default:
			apiRepo, ok := slotRepos[name]
			if !ok {
				c.ApiAvailability = availabilityNotQueried
			} else if apiRepo == nil {
				c.ApiAvailability = availabilityNotFound
			} else {
				c.ApiAvailability = availabilityReturned
				c.ApiNameWithOwner = apiNameWithOwner(apiRepo)
			}
		}
	}

	// Network-only repositories, such as fork parents with no local remote.
	if network != nil && networkErr == nil {
		for _, apiRepo := range networkAllRepositories(network) {
			if hasEquivalentCandidate(out, apiRepo) {
				continue
			}
			out = append(out, candidateReport{
				NameWithOwner:    apiNameWithOwner(apiRepo),
				Host:             apiRepo.RepoHost(),
				Remote:           "",
				RemoteExists:     false,
				Considered:       true,
				ApiAvailability:  availabilityReturned,
				ApiNameWithOwner: apiNameWithOwner(apiRepo),
				Reasons:          []string{"not_backed_by_a_git_remote"},
			})
		}
	}

	// Stale records belonging to remotes that no longer exist.
	if recordsOK {
		staleNames := make([]string, 0)
		for name := range records {
			if _, ok := candByRemote[name]; ok {
				continue
			}
			staleNames = append(staleNames, name)
		}
		sort.Strings(staleNames)
		for _, name := range staleNames {
			value := records[name]
			c := candidateReport{
				Remote:          name,
				RemoteExists:    false,
				ResolutionValue: value,
				Considered:      false,
				ApiAvailability: availabilityNotQueried,
				Reasons:         []string{"remote_no_longer_exists"},
			}
			if value != "base" {
				if repo, err := ghrepo.FromFullName(value); err == nil {
					c.NameWithOwner = ghrepo.FullName(repo)
					c.Host = repo.RepoHost()
				}
			}
			out = append(out, c)
		}
	}

	return out
}

func exclusionReason(host string, authedHosts []string) string {
	if host == "" {
		return "not_a_github_repository"
	}
	for _, authed := range authedHosts {
		if strings.EqualFold(authed, host) {
			return "excluded_by_host_selector"
		}
	}
	return "host_not_authenticated"
}

func hasEquivalentCandidate(cands []candidateReport, apiRepo *api.Repository) bool {
	for _, c := range cands {
		if c.Host == apiRepo.RepoHost() && strings.EqualFold(c.NameWithOwner, apiNameWithOwner(apiRepo)) {
			return true
		}
	}
	return false
}

func apiNameWithOwner(r *api.Repository) string {
	return ghrepo.FullName(r)
}

// networkAllRepositories returns every repository the network returned,
// including fork parents, de-duplicating repositories with the same host and
// full name.
func networkAllRepositories(network *api.RepoNetworkResult) []*api.Repository {
	var out []*api.Repository
	seen := map[string]bool{}
	add := func(r *api.Repository) {
		if r == nil {
			return
		}
		key := r.RepoHost() + "/" + ghrepo.FullName(r)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, r)
	}
	for _, r := range network.Repositories {
		if r == nil {
			continue
		}
		add(r.Parent)
		add(r)
	}
	return out
}

func findOrCreateSelectedCandidate(cands *[]candidateReport, repo ghrepo.Interface) *candidateReport {
	for i := range *cands {
		c := &(*cands)[i]
		if c.Host == repo.RepoHost() && strings.EqualFold(c.NameWithOwner, ghrepo.FullName(repo)) {
			return c
		}
	}
	*cands = append(*cands, candidateReport{
		NameWithOwner:   ghrepo.FullName(repo),
		Host:            repo.RepoHost(),
		Remote:          "",
		RemoteExists:    false,
		Considered:      true,
		ApiAvailability: availabilityNotQueried,
		Reasons:         []string{"not_backed_by_a_git_remote"},
	})
	return &(*cands)[len(*cands)-1]
}

// applyCandidateReasons adds rule-specific non-selection reasons. Structural
// reasons such as host authentication were added during assembly.
func applyCandidateReasons(cands *[]candidateReport, primary context.Outcome, override bool) {
	for i := range *cands {
		c := &(*cands)[i]
		if c.Selected {
			continue
		}

		if c.RemoteExists && c.Considered && c.ResolutionValue == "" && c.NameWithOwner != "" {
			c.Reasons = append(c.Reasons, "no_resolution_record")
		}

		if c.ApiAvailability == availabilityNotFound {
			c.Reasons = append(c.Reasons, "api_did_not_return_repository")
		}

		switch {
		case primary.Rule == context.RuleAmbiguous && c.Considered && c.ApiAvailability == availabilityReturned:
			c.Reasons = append(c.Reasons, "multiple_candidates_manual_selection_required")
		case override && c.Considered:
			c.Reasons = append(c.Reasons, "explicit_override_in_effect")
		case primary.Rule == context.RuleNonInteractiveFallback || primary.Rule == context.RuleNetworkEmptyFallback:
			if c.Considered && c.Remote != "" {
				c.Reasons = append(c.Reasons, "not_first_in_remote_sort_order")
			}
		}
	}
}

func candidateScope(raws []rawEntry, filtered context.Remotes) string {
	if len(raws) == len(filtered) {
		return scopeAll
	}
	return scopeHostFiltered
}

func buildOutcomeReport(outcome context.Outcome, resolved *context.ResolvedRemotes) outcomeReport {
	if resolved == nil {
		return outcomeReport{Available: false}
	}
	report := outcomeReport{
		Available: true,
		Rule:      string(outcome.Rule),
	}
	if outcome.Repo != nil {
		report.Repository = newRepositoryRef(outcome.Repo)
	}
	if outcome.Err != nil {
		report.Error = outcome.Err.Error()
	}
	return report
}

// buildDefaultRecords reports every gh-resolved config key, including records
// the active resolution did not apply.
func buildDefaultRecords(
	records map[string]string,
	recordsOK bool,
	rawByName map[string]rawEntry,
	filtered context.Remotes,
	network *api.RepoNetworkResult,
	networkFailed bool,
	override bool,
	primary context.Outcome,
) []defaultRecordReport {
	if !recordsOK || len(records) == 0 {
		return []defaultRecordReport{}
	}

	// The chain applies the first resolved remote in filtered order.
	effectiveRemote := ""
	if !override {
		for _, rem := range filtered {
			if rem.Resolved != "" {
				effectiveRemote = rem.Name
				break
			}
		}
	}

	filteredIndex := map[string]int{}
	for i, rem := range filtered {
		filteredIndex[rem.Name] = i
	}

	names := make([]string, 0, len(records))
	for name := range records {
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]defaultRecordReport, 0, len(names))
	for _, name := range names {
		value := records[name]
		record := defaultRecordReport{
			Remote:       name,
			Value:        value,
			RemoteExists: false,
			Issues:       []string{},
		}

		raw, rawExists := rawByName[name]
		record.RemoteExists = rawExists
		if !rawExists {
			record.Valid = new(false)
			record.Issues = append(record.Issues, "remote_no_longer_exists")
			out = append(out, record)
			continue
		}

		record.Effective = name == effectiveRemote
		if override {
			record.Issues = append(record.Issues, "not_effective_explicit_override")
		}

		target := recordTarget(value, raw)
		record.Target = target

		if _, inFiltered := filteredIndex[name]; !inFiltered {
			record.Issues = append(record.Issues, "remote_excluded_by_host_selector")
		}

		// Validate the target against the API network result.
		switch {
		case networkFailed || network == nil:
			record.Valid = nil
			record.Issues = append(record.Issues, "validity_unverified")
		case target == nil:
			record.Valid = new(false)
			record.Issues = append(record.Issues, "target_unparseable")
		default:
			valid, issue := validateTarget(name, value, target, filteredIndex[name], network)
			record.Valid = new(valid)
			if issue != "" {
				record.Issues = append(record.Issues, issue)
			}
		}

		if target != nil && primary.Repo != nil &&
			(target.Host != primary.Repo.RepoHost() || !strings.EqualFold(target.NameWithOwner, ghrepo.FullName(primary.Repo))) {
			record.Issues = append(record.Issues, "target_differs_from_resolution_result")
		}

		out = append(out, record)
	}
	return out
}

func recordTarget(value string, raw rawEntry) *repositoryRef {
	if raw.repo == nil {
		return nil
	}
	if value == "base" {
		return newRepositoryRef(raw.repo)
	}
	parsed, err := ghrepo.FromFullName(value)
	if err != nil {
		return nil
	}
	// The chain resolves the value against the remote's host.
	return newRepositoryRef(ghrepo.NewWithHost(parsed.RepoOwner(), parsed.RepoName(), raw.repo.RepoHost()))
}

func validateTarget(remoteName, value string, target *repositoryRef, filteredIndex int, network *api.RepoNetworkResult) (bool, string) {
	if value == "base" {
		if filteredIndex < 0 || filteredIndex >= len(network.Repositories) {
			return false, "repository_not_found"
		}
		apiRepo := network.Repositories[filteredIndex]
		if apiRepo == nil {
			return false, "repository_not_found"
		}
		if !strings.EqualFold(apiNameWithOwner(apiRepo), target.NameWithOwner) {
			return false, "repository_renamed"
		}
		return true, ""
	}

	for _, apiRepo := range networkAllRepositories(network) {
		if apiRepo.RepoHost() == target.Host && strings.EqualFold(apiNameWithOwner(apiRepo), target.NameWithOwner) {
			return true, ""
		}
	}
	return false, "repository_not_found"
}

func determineFailure(
	cfgErr, httpClientErr, filteredErr, rawErr, r2rErr, networkErr error,
	primary context.Outcome, canPrompt bool,
) *failureReport {
	switch {
	case cfgErr != nil:
		return &failureReport{Step: "configuration", Message: cfgErr.Error()}
	case httpClientErr != nil:
		return &failureReport{Step: "http_client", Message: httpClientErr.Error()}
	case r2rErr != nil:
		return &failureReport{Step: "explicit_override", Message: r2rErr.Error()}
	case filteredErr != nil:
		return &failureReport{Step: "remote_selection", Message: filteredErr.Error()}
	case rawErr != nil:
		return &failureReport{Step: "remote_discovery", Message: rawErr.Error()}
	case canPrompt && networkErr != nil:
		return &failureReport{Step: "network_resolution", Message: networkErr.Error()}
	case primary.Rule == context.RuleAmbiguous:
		return &failureReport{
			Step:    "disambiguation",
			Message: "multiple candidate repositories; run `gh repo set-default` to select one",
		}
	case primary.Err != nil:
		return &failureReport{Step: strings.ToLower(string(primary.Rule)), Message: primary.Err.Error()}
	default:
		return nil
	}
}

//go:fix inline
func boolPtr(v bool) *bool {
	return new(v)
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func humanRule(rule string) string {
	switch context.Rule(rule) {
	case context.RuleExplicitOverride:
		return "explicit repository override (--repo or GH_REPO)"
	case context.RuleResolvedBase:
		return "default repository record resolving to its own remote"
	case context.RuleResolvedRepository:
		return "default repository record naming a specific repository"
	case context.RuleNonInteractiveFallback:
		return "fallback to the first sorted remote because prompting is unavailable"
	case context.RuleNetwork:
		return "API resolved a single repository"
	case context.RuleNetworkEmptyFallback:
		return "fallback to the first sorted remote because the API returned no repositories"
	default:
		return rule
	}
}

func printHumanReport(io *iostreams.IOStreams, rep *report) {
	cs := io.ColorScheme()
	out := io.Out

	fmt.Fprintf(out, "Base repository resolution for %s\n\n", rep.CWD)

	fmt.Fprintf(out, "Status: %s\n", rep.Status)
	if rep.Selected != nil {
		fmt.Fprintf(out, "Selected: %s on %s", rep.Selected.NameWithOwner, rep.Selected.Host)
		if rep.SelectedRemote != "" {
			fmt.Fprintf(out, " via remote %q", rep.SelectedRemote)
		}
		fmt.Fprintln(out)
		fmt.Fprintf(out, "Rule: %s\n", humanRule(rep.Rule))
		if rep.FallbackInEffect {
			fmt.Fprintln(out, "Note: this invocation fell back to the first sorted remote; no default repository record was used.")
		}
	}

	fmt.Fprintln(out)
	fmt.Fprintf(out, "Explicit override: %s\n", overrideSummary(rep.Override))
	fmt.Fprintf(out, "Hosts: authenticated %s; default %s (source: %s)\n",
		strings.Join(rep.Hosts.Authenticated, ", "), rep.Hosts.DefaultHost, rep.Hosts.DefaultSource)

	if len(rep.DefaultRecords) > 0 {
		fmt.Fprintln(out, "Default repository records:")
		for _, record := range rep.DefaultRecords {
			fmt.Fprintf(out, "  remote %q gh-resolved=%s", record.Remote, record.Value)
			if record.Target != nil {
				fmt.Fprintf(out, " -> %s on %s", record.Target.NameWithOwner, record.Target.Host)
			}
			fmt.Fprintf(out, " [%s]\n", recordSummary(record))
			for _, issue := range record.Issues {
				fmt.Fprintf(out, "    - %s\n", issue)
			}
		}
	}

	scopeText := "all remotes in this directory"
	if rep.Scope.Scope == scopeHostFiltered {
		scopeText = "a host-filtered subset of this directory's remotes"
	}
	fmt.Fprintf(out, "Candidates (%s; %d of %d remotes considered):\n",
		scopeText, rep.Scope.ConsideredRemotes, rep.Scope.TotalRemotes)
	for _, c := range rep.Candidates {
		mark := "  "
		if c.Selected {
			mark = cs.SuccessIcon() + " "
		}
		name := c.NameWithOwner
		if name == "" {
			if c.ResolutionValue != "" {
				name = "(gh-resolved: " + c.ResolutionValue + ")"
			} else {
				name = "(unrecognized remote URL)"
			}
		}
		fmt.Fprintf(out, "%s%-30s %-18s remote=%-10s api=%s\n",
			mark, name, c.Host, remoteLabel(c), c.ApiAvailability)
		for _, reason := range c.Reasons {
			fmt.Fprintf(out, "      not selected: %s\n", reason)
		}
	}

	fmt.Fprintf(out, "Interactive resolution:     %s\n", outcomeSummary(rep.Interactive))
	fmt.Fprintf(out, "Non-interactive resolution: %s\n", outcomeSummary(rep.NonInteractive))

	if rep.Failure != nil {
		fmt.Fprintf(out, "Failure step: %s\n", rep.Failure.Step)
		fmt.Fprintf(out, "Failure: %s\n", rep.Failure.Message)
	}
}

func overrideSummary(o overrideReport) string {
	if !o.Present {
		return "none"
	}
	return fmt.Sprintf("%s via %s (valid: %s)", o.Value, o.Source, humanValid(o.Valid))
}

func humanValid(v *bool) string {
	if v == nil {
		return "unverified"
	}
	if *v {
		return "true"
	}
	return "false"
}

func recordSummary(r defaultRecordReport) string {
	parts := []string{fmt.Sprintf("valid=%s", humanValid(r.Valid))}
	if r.Effective {
		parts = append(parts, "applied")
	}
	if !r.RemoteExists {
		parts = append(parts, "remote missing")
	}
	return strings.Join(parts, ", ")
}

func remoteLabel(c candidateReport) string {
	if c.Remote == "" {
		return "(none)"
	}
	if !c.RemoteExists {
		return c.Remote + " (missing)"
	}
	return c.Remote
}

func outcomeSummary(o outcomeReport) string {
	if !o.Available {
		return "unavailable: resolution chain could not run"
	}
	if o.Error != "" {
		return fmt.Sprintf("failed via %s: %s", o.Rule, o.Error)
	}
	if o.Repository == nil {
		return "no repository (rule: " + o.Rule + ")"
	}
	return fmt.Sprintf("%s via %s", o.Repository.NameWithOwner, humanRule(o.Rule))
}
