// TODO: rename this package to avoid clash with stdlib
package context

import (
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/pkg/iostreams"
)

// Cap the number of git remotes to look up, since the user might have an
// unusually large number of git remotes.
const DefaultRemotesForLookup = 5

func ResolveRemotesToRepos(remotes Remotes, client *api.Client, base string) (*ResolvedRemotes, error) {
	sort.Stable(remotes)

	result := &ResolvedRemotes{
		remotes:   remotes,
		apiClient: client,
	}

	var baseOverride ghrepo.Interface
	if base != "" {
		var err error
		baseOverride, err = ghrepo.FromFullName(base)
		if err != nil {
			return result, err
		}
		result.baseOverride = baseOverride
	}

	return result, nil
}

func resolveNetwork(result *ResolvedRemotes, remotesForLookup int) error {
	var repos []ghrepo.Interface
	for _, r := range result.remotes {
		repos = append(repos, r)
		if len(repos) == remotesForLookup {
			break
		}
	}

	networkResult, err := api.RepoNetwork(result.apiClient, repos)
	result.network = &networkResult
	result.networkErr = err
	return err
}

type ResolvedRemotes struct {
	baseOverride ghrepo.Interface
	remotes      Remotes
	network      *api.RepoNetworkResult
	networkErr   error
	apiClient    *api.Client
}

// Rule identifies the category of resolution rule that produces a base
// repository outcome.
type Rule string

const (
	// RuleExplicitOverride means a repository was supplied with --repo or
	// GH_REPO and short-circuits every other rule.
	RuleExplicitOverride Rule = "explicit_override"
	// RuleResolvedBase means a remote's gh-resolved value was "base", so the
	// remote itself is the base repository.
	RuleResolvedBase Rule = "resolved_base"
	// RuleResolvedRepository means a remote's gh-resolved value was a specific
	// OWNER/REPO, which is the base repository regardless of remotes.
	RuleResolvedRepository Rule = "resolved_repository"
	// RuleNonInteractiveFallback means the invocation could not prompt, so the
	// first sorted remote is used for backwards compatibility.
	RuleNonInteractiveFallback Rule = "non_interactive_fallback"
	// RuleNetwork means the API resolved a single repository for the remotes.
	RuleNetwork Rule = "network_single"
	// RuleNetworkEmptyFallback means the API returned no repositories, so the
	// first sorted remote is used.
	RuleNetworkEmptyFallback Rule = "network_empty_fallback"
	// RuleAmbiguous means the API returned multiple repositories and the user
	// must designate one with `repo set-default`.
	RuleAmbiguous Rule = "ambiguous"
)

// Outcome is the base repository produced for one promptability setting. When
// Rule is RuleAmbiguous, both Repo and Err are nil. When Err is non-nil, the
// rule category identifies which step failed whenever a rule had been reached.
type Outcome struct {
	Rule Rule
	Repo ghrepo.Interface
	Err  error
}

// Resolve applies the base repository rules without prompting or printing,
// unlike BaseRepo. canPrompt reports whether the simulated invocation is able
// to interactively prompt the user.
func (r *ResolvedRemotes) Resolve(canPrompt bool) Outcome {
	if r.baseOverride != nil {
		return Outcome{Rule: RuleExplicitOverride, Repo: r.baseOverride}
	}

	if len(r.remotes) == 0 {
		return Outcome{Err: errors.New("no git remotes")}
	}

	// if any of the remotes already has a resolution, respect that
	for _, rem := range r.remotes {
		if rem.Resolved == "base" {
			return Outcome{Rule: RuleResolvedBase, Repo: rem}
		} else if rem.Resolved != "" {
			repo, err := ghrepo.FromFullName(rem.Resolved)
			if err != nil {
				return Outcome{Rule: RuleResolvedRepository, Err: err}
			}
			return Outcome{Rule: RuleResolvedRepository, Repo: ghrepo.NewWithHost(repo.RepoOwner(), repo.RepoName(), rem.RepoHost())}
		}
	}

	if !canPrompt {
		// we cannot prompt, so just resort to the 1st remote
		return Outcome{Rule: RuleNonInteractiveFallback, Repo: r.remotes[0]}
	}

	repos, err := r.NetworkRepos(DefaultRemotesForLookup)
	if err != nil {
		return Outcome{Err: err}
	}

	switch len(repos) {
	case 0:
		return Outcome{Rule: RuleNetworkEmptyFallback, Repo: r.remotes[0]}
	case 1:
		return Outcome{Rule: RuleNetwork, Repo: repos[0]}
	default:
		return Outcome{Rule: RuleAmbiguous}
	}
}

func (r *ResolvedRemotes) BaseRepo(io *iostreams.IOStreams) (ghrepo.Interface, error) {
	outcome := r.Resolve(io.CanPrompt())
	if outcome.Rule == RuleAmbiguous {
		cs := io.ColorScheme()

		fmt.Fprintf(io.ErrOut,
			"%s No default remote repository has been set. To learn more about the default repository, run: gh repo set-default --help\n",
			cs.FailureIcon())

		fmt.Fprintln(io.Out)

		return nil, errors.New(
			"please run `gh repo set-default` to select a default remote repository.")
	}
	return outcome.Repo, outcome.Err
}

func (r *ResolvedRemotes) HeadRepos() ([]*api.Repository, error) {
	if r.network == nil {
		err := resolveNetwork(r, DefaultRemotesForLookup)
		if err != nil {
			return nil, err
		}
	}

	var results []*api.Repository
	var ids []string // Check if repo duplicates
	for _, repo := range r.network.Repositories {
		if repo != nil && repo.ViewerCanPush() && !slices.Contains(ids, repo.ID) {
			results = append(results, repo)
			ids = append(ids, repo.ID)
		}
	}
	return results, nil
}

// NetworkResult fetches the raw repository network result for diagnostic
// inspection, using the same cached fetch as NetworkRepos. The Repositories
// slice aligns positionally with the first remotesForLookup remotes; nil
// entries mean the API did not resolve that remote's repository. Pass a value
// of 0 to fetch info on all remotes.
func (r *ResolvedRemotes) NetworkResult(remotesForLookup int) (*api.RepoNetworkResult, error) {
	if r.network == nil {
		err := resolveNetwork(r, remotesForLookup)
		return r.network, err
	}
	return r.network, r.networkErr
}

// NetworkRepos fetches info about remotes for the network of repos.
// Pass a value of 0 to fetch info on all remotes.
func (r *ResolvedRemotes) NetworkRepos(remotesForLookup int) ([]*api.Repository, error) {
	if r.network == nil {
		err := resolveNetwork(r, remotesForLookup)
		if err != nil {
			return nil, err
		}
	}

	var repos []*api.Repository
	repoMap := map[string]bool{}

	add := func(r *api.Repository) {
		fn := ghrepo.FullName(r)
		if _, ok := repoMap[fn]; !ok {
			repoMap[fn] = true
			repos = append(repos, r)
		}
	}

	for _, repo := range r.network.Repositories {
		if repo == nil {
			continue
		}
		if repo.Parent != nil {
			add(repo.Parent)
		}
		add(repo)
	}

	return repos, nil
}

// RemoteForRepo finds the git remote that points to a repository
func (r *ResolvedRemotes) RemoteForRepo(repo ghrepo.Interface) (*Remote, error) {
	for _, remote := range r.remotes {
		if ghrepo.IsSame(remote, repo) {
			return remote, nil
		}
	}
	return nil, errors.New("not found")
}
