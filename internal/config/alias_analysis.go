package config

import (
	"cmp"
	"slices"
	"strings"
	"sync"

	"github.com/cli/cli/v2/internal/gh"
	ghConfig "github.com/cli/go-gh/v2/pkg/config"
	"github.com/google/shlex"
)

// MaxAliasExpansionDepth is the maximum number of alias-to-alias expansions that
// are followed while resolving an alias. It guards against circular and
// pathologically deep alias chains, both when the configuration is loaded and
// when an alias is executed.
const MaxAliasExpansionDepth = 10

// AnalyzeAliases validates every alias expansion. It identifies self referential
// and circular alias chains, computes the expansion depth of each finite alias in
// alias hops, and flags aliases whose depth exceeds MaxAliasExpansionDepth.
//
// The expansion graph is built from strings alone: an edge from one alias to
// another exists when the second alias's name tokens are the leading command
// path of the first alias's expansion, mirroring how cobra resolves the
// expansion to a command. The longest matching alias name wins, matching
// cobra's deepest command resolution.
func AnalyzeAliases(aliases map[string]string) gh.AliasAnalysis {
	graph, names := buildAliasGraph(aliases)

	const (
		white = iota
		gray
		black
	)
	colors := map[string]int{}
	cyclic := map[string]bool{}
	var issues []gh.AliasIssue

	for _, start := range names {
		if colors[start] != white {
			continue
		}

		path := []string{}
		cur := start
		for {
			switch colors[cur] {
			case black:
				if cyclic[cur] && len(path) > 0 {
					// The path leads into a circular chain found by an earlier walk.
					issues = append(issues, leadInIssues(path, cycleChainFor(issues, cur))...)
					for _, n := range path {
						cyclic[n] = true
					}
				}
				for _, n := range path {
					colors[n] = black
				}
				cur = ""
			case gray:
				idx := slices.Index(path, cur)
				cycle := slices.Clone(path[idx:])

				// Aliases leading into the cycle also never finish expanding.
				for i := range idx {
					chain := append(slices.Clone(path[i:]), cur)
					issues = append(issues, gh.AliasIssue{
						Name:  path[i],
						Kind:  gh.AliasCircularReference,
						Chain: chain,
					})
					cyclic[path[i]] = true
				}

				for _, n := range cycle {
					cyclic[n] = true
					kind := gh.AliasCircularReference
					if len(cycle) == 1 {
						kind = gh.AliasSelfReference
					}
					issues = append(issues, gh.AliasIssue{
						Name:  n,
						Kind:  kind,
						Chain: cycleChain(cycle, n),
					})
				}

				for _, n := range path {
					colors[n] = black
				}
				cur = ""
			default:
				colors[cur] = gray
				path = append(path, cur)
				target := graph.edges[cur]
				if target == "" {
					for _, n := range path {
						colors[n] = black
					}
					cur = ""
				} else {
					cur = target
				}
			}

			if cur == "" {
				break
			}
		}
	}

	depths := computeDepths(graph, names, cyclic)

	for _, name := range names {
		if cyclic[name] {
			continue
		}
		depth := depths[name]
		if depth <= MaxAliasExpansionDepth {
			continue
		}
		chain := []string{name}
		for target := graph.edges[name]; target != ""; target = graph.edges[chain[len(chain)-1]] {
			chain = append(chain, target)
		}
		issues = append(issues, gh.AliasIssue{
			Name:  name,
			Kind:  gh.AliasExpansionDepthExceeded,
			Chain: chain,
			Depth: depth,
		})
	}

	slices.SortStableFunc(issues, func(a, b gh.AliasIssue) int {
		return cmp.Compare(a.Name, b.Name)
	})

	return gh.AliasAnalysis{
		Issues: issues,
		Depths: depths,
	}
}

// aliasGraph is the directed graph of alias expansions. Each alias has at most
// one outgoing edge: the alias its expansion resolves to first, or no edge when
// the expansion is terminal (a command or shell expansion).
type aliasGraph struct {
	edges map[string]string
}

func buildAliasGraph(aliases map[string]string) (aliasGraph, []string) {
	tokens := map[string][]string{}
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		toks, err := shlex.Split(name)
		if err != nil || len(toks) == 0 {
			continue
		}
		names = append(names, name)
		tokens[name] = toks
	}
	slices.Sort(names)

	graph := aliasGraph{edges: map[string]string{}}
	for _, name := range names {
		expansion := aliases[name]
		if strings.HasPrefix(expansion, "!") {
			continue
		}
		expToks, err := shlex.Split(expansion)
		if err != nil {
			continue
		}

		target := ""
		for _, candidate := range names {
			candToks := tokens[candidate]
			if len(candToks) > len(expToks) {
				continue
			}
			if !slices.Equal(candToks, expToks[:len(candToks)]) {
				continue
			}
			if target == "" || len(candToks) > len(tokens[target]) {
				target = candidate
			}
		}
		graph.edges[name] = target
	}

	return graph, names
}

// computeDepths returns the expansion depth in alias hops for every non-cyclic
// alias. Terminal expansions have depth 0. The walk is iterative so that long
// chains cannot exhaust the stack.
func computeDepths(graph aliasGraph, names []string, cyclic map[string]bool) map[string]int {
	depths := map[string]int{}
	for _, name := range names {
		if cyclic[name] {
			continue
		}
		if _, done := depths[name]; done {
			continue
		}

		chain := []string{}
		cur := name
		for {
			if d, done := depths[cur]; done {
				for _, c := range slices.Backward(chain) {
					d++
					depths[c] = d
				}
				break
			}

			target := graph.edges[cur]
			if target == "" {
				depths[cur] = 0
				d := 0
				for _, c := range slices.Backward(chain) {
					d++
					depths[c] = d
				}
				break
			}

			chain = append(chain, cur)
			cur = target
		}
	}
	return depths
}

// cycleChain rotates cycle so it starts and ends with name, e.g. [b c a b].
func cycleChain(cycle []string, name string) []string {
	idx := slices.Index(cycle, name)
	rotated := append(slices.Clone(cycle[idx:]), cycle[:idx]...)
	return append(rotated, name)
}

// cycleChainFor returns the demonstration chain recorded for a cyclic alias.
func cycleChainFor(issues []gh.AliasIssue, name string) []string {
	for _, issue := range issues {
		if issue.Name == name {
			return issue.Chain
		}
	}
	return []string{name, name}
}

// leadInIssues builds circular reference issues for a path that leads into an
// already known circular chain.
func leadInIssues(path []string, cycleChain []string) []gh.AliasIssue {
	issues := make([]gh.AliasIssue, 0, len(path))
	for i, n := range path {
		chain := append(slices.Clone(path[i:]), cycleChain...)
		issues = append(issues, gh.AliasIssue{
			Name:  n,
			Kind:  gh.AliasCircularReference,
			Chain: chain,
		})
	}
	return issues
}

type AliasConfig struct {
	cfg *ghConfig.Config

	analysisOnce sync.Once
	analysis     gh.AliasAnalysis
}

func (a *AliasConfig) Analyze() gh.AliasAnalysis {
	a.analysisOnce.Do(func() {
		a.analysis = AnalyzeAliases(a.All())
	})
	return a.analysis
}
