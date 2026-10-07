package config

import (
	"fmt"
	"testing"

	"github.com/cli/cli/v2/internal/gh"
	ghConfig "github.com/cli/go-gh/v2/pkg/config"
	"github.com/stretchr/testify/require"
)

func TestAnalyzeAliases(t *testing.T) {
	tests := []struct {
		name       string
		aliases    map[string]string
		wantIssues map[string]gh.AliasIssue
		wantDepths map[string]int
	}{
		{
			name:       "no aliases",
			aliases:    map[string]string{},
			wantIssues: map[string]gh.AliasIssue{},
			wantDepths: map[string]int{},
		},
		{
			name: "terminal alias has depth zero",
			aliases: map[string]string{
				"co": "pr checkout",
			},
			wantIssues: map[string]gh.AliasIssue{},
			wantDepths: map[string]int{"co": 0},
		},
		{
			name: "shell alias has depth zero",
			aliases: map[string]string{
				"hi": "!echo hi",
			},
			wantIssues: map[string]gh.AliasIssue{},
			wantDepths: map[string]int{"hi": 0},
		},
		{
			name: "chain one hop deep",
			aliases: map[string]string{
				"a":  "co",
				"co": "pr checkout",
			},
			wantIssues: map[string]gh.AliasIssue{},
			wantDepths: map[string]int{"a": 1, "co": 0},
		},
		{
			name: "self referential alias",
			aliases: map[string]string{
				"a": "a",
			},
			wantIssues: map[string]gh.AliasIssue{
				"a": {
					Name:  "a",
					Kind:  gh.AliasSelfReference,
					Chain: []string{"a", "a"},
				},
			},
			wantDepths: map[string]int{},
		},
		{
			name: "multiword self referential alias",
			aliases: map[string]string{
				"repo x": "repo x",
			},
			wantIssues: map[string]gh.AliasIssue{
				"repo x": {
					Name:  "repo x",
					Kind:  gh.AliasSelfReference,
					Chain: []string{"repo x", "repo x"},
				},
			},
			wantDepths: map[string]int{},
		},
		{
			name: "mutual circular reference",
			aliases: map[string]string{
				"a": "b",
				"b": "a",
			},
			wantIssues: map[string]gh.AliasIssue{
				"a": {
					Name:  "a",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"a", "b", "a"},
				},
				"b": {
					Name:  "b",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"b", "a", "b"},
				},
			},
			wantDepths: map[string]int{},
		},
		{
			name: "three node circular reference",
			aliases: map[string]string{
				"a": "b",
				"b": "c",
				"c": "a",
			},
			wantIssues: map[string]gh.AliasIssue{
				"a": {
					Name:  "a",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"a", "b", "c", "a"},
				},
				"b": {
					Name:  "b",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"b", "c", "a", "b"},
				},
				"c": {
					Name:  "c",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"c", "a", "b", "c"},
				},
			},
			wantDepths: map[string]int{},
		},
		{
			name: "alias leads into a circular chain",
			aliases: map[string]string{
				"a": "b",
				"b": "a",
				"l": "a",
			},
			wantIssues: map[string]gh.AliasIssue{
				"a": {
					Name:  "a",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"a", "b", "a"},
				},
				"b": {
					Name:  "b",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"b", "a", "b"},
				},
				"l": {
					Name:  "l",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"l", "a", "b", "a"},
				},
			},
			wantDepths: map[string]int{},
		},
		{
			name: "circular aliases do not affect unrelated aliases",
			aliases: map[string]string{
				"a":    "b",
				"b":    "a",
				"good": "pr status",
			},
			wantIssues: map[string]gh.AliasIssue{
				"a": {
					Name:  "a",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"a", "b", "a"},
				},
				"b": {
					Name:  "b",
					Kind:  gh.AliasCircularReference,
					Chain: []string{"b", "a", "b"},
				},
			},
			wantDepths: map[string]int{"good": 0},
		},
		{
			name:       "chain at exactly the maximum depth is allowed",
			aliases:    chainAliases(MaxAliasExpansionDepth),
			wantIssues: map[string]gh.AliasIssue{},
			wantDepths: chainDepths(MaxAliasExpansionDepth),
		},
		{
			name:    "chain one beyond the maximum depth is flagged",
			aliases: chainAliases(MaxAliasExpansionDepth + 1),
			wantIssues: map[string]gh.AliasIssue{
				"a00": {
					Name:  "a00",
					Kind:  gh.AliasExpansionDepthExceeded,
					Chain: chainNames(MaxAliasExpansionDepth + 1),
					Depth: MaxAliasExpansionDepth + 1,
				},
			},
			wantDepths: chainDepths(MaxAliasExpansionDepth + 1),
		},
		{
			name: "quoted expansion tokens resolve edges",
			aliases: map[string]string{
				"a": `"pr" status`,
			},
			wantIssues: map[string]gh.AliasIssue{},
			wantDepths: map[string]int{"a": 0},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			analysis := AnalyzeAliases(tt.aliases)

			require.Len(t, analysis.Issues, len(tt.wantIssues))
			gotIssues := map[string]gh.AliasIssue{}
			for _, issue := range analysis.Issues {
				gotIssues[issue.Name] = issue
			}
			for name, want := range tt.wantIssues {
				got, ok := gotIssues[name]
				require.True(t, ok, "missing issue for %q", name)
				require.Equal(t, want.Kind, got.Kind)
				require.Equal(t, want.Chain, got.Chain)
				require.Equal(t, want.Depth, got.Depth)
			}

			for name, wantDepth := range tt.wantDepths {
				gotDepth, ok := analysis.Depths[name]
				require.True(t, ok, "missing depth for %q", name)
				require.Equal(t, wantDepth, gotDepth)
			}
			require.Len(t, analysis.Depths, len(tt.wantDepths))
		})
	}
}

func TestAliasConfigAnalyzeIsCached(t *testing.T) {
	aliasCfg := &AliasConfig{cfg: ghConfig.ReadFromString("")}
	require.Equal(t, aliasCfg.Analyze(), aliasCfg.Analyze())
}

// chainAliases builds hops aliases a00 -> a01 -> ... -> a(n-1) -> an, with an
// pointing at the terminal command "pr status".
func chainAliases(hoppings int) map[string]string {
	aliases := map[string]string{}
	for i := range hoppings {
		aliases[fmt.Sprintf("a%02d", i)] = fmt.Sprintf("a%02d", i+1)
	}
	aliases[fmt.Sprintf("a%02d", hoppings)] = "pr status"
	return aliases
}

// chainNames returns the alias names of chainAliases in traversal order,
// including the terminal alias.
func chainNames(hoppings int) []string {
	names := make([]string, 0, hoppings+1)
	for i := 0; i <= hoppings; i++ {
		names = append(names, fmt.Sprintf("a%02d", i))
	}
	return names
}

// chainDepths returns the per-alias depths expected for chainAliases.
func chainDepths(hoppings int) map[string]int {
	depths := map[string]int{}
	for i := 0; i <= hoppings; i++ {
		depths[fmt.Sprintf("a%02d", i)] = hoppings - i
	}
	return depths
}
