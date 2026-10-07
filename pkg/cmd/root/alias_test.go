package root

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExpandAlias(t *testing.T) {
	tests := []struct {
		name         string
		expansion    string
		args         []string
		wantExpanded []string
		wantErr      string
	}{
		{
			name:         "no expansion",
			expansion:    "pr status",
			args:         []string{},
			wantExpanded: []string{"pr", "status"},
		},
		{
			name:         "adding arguments after expansion",
			expansion:    "pr checkout",
			args:         []string{"123"},
			wantExpanded: []string{"pr", "checkout", "123"},
		},
		{
			name:      "not enough arguments for expansion",
			expansion: `issue list --author="$1" --label="$2"`,
			args:      []string{},
			wantErr:   `not enough arguments for alias: issue list --author="$1" --label="$2"`,
		},
		{
			name:      "not enough arguments for expansion 2",
			expansion: `issue list --author="$1" --label="$2"`,
			args:      []string{"vilmibm"},
			wantErr:   `not enough arguments for alias: issue list --author="vilmibm" --label="$2"`,
		},
		{
			name:         "satisfy expansion arguments",
			expansion:    `issue list --author="$1" --label="$2"`,
			args:         []string{"vilmibm", "help wanted"},
			wantExpanded: []string{"issue", "list", "--author=vilmibm", "--label=help wanted"},
		},
		{
			name:         "mixed positional and non-positional arguments",
			expansion:    `issue list --author="$1" --label="$2"`,
			args:         []string{"vilmibm", "epic", "-R", "monalisa/testing"},
			wantExpanded: []string{"issue", "list", "--author=vilmibm", "--label=epic", "-R", "monalisa/testing"},
		},
		{
			name:         "dollar in expansion",
			expansion:    `issue list --author="$1" --assignee="$1"`,
			args:         []string{"$coolmoney$"},
			wantExpanded: []string{"issue", "list", "--author=$coolmoney$", "--assignee=$coolmoney$"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotExpanded, err := expandAlias(tt.expansion, tt.args)
			if tt.wantErr != "" {
				assert.Nil(t, gotExpanded)
				assert.EqualError(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantExpanded, gotExpanded)
		})
	}
}

func TestExpandShellAlias(t *testing.T) {
	findShFunc := func() (string, error) {
		return "/usr/bin/sh", nil
	}
	tests := []struct {
		name         string
		expansion    string
		args         []string
		findSh       func() (string, error)
		wantExpanded []string
		wantErr      string
	}{
		{
			name:         "simple expansion",
			expansion:    "!git branch --show-current",
			args:         []string{},
			findSh:       findShFunc,
			wantExpanded: []string{"/usr/bin/sh", "-c", "git branch --show-current"},
		},
		{
			name:         "adding arguments after expansion",
			expansion:    "!git branch checkout",
			args:         []string{"123"},
			findSh:       findShFunc,
			wantExpanded: []string{"/usr/bin/sh", "-c", "git branch checkout", "--", "123"},
		},
		{
			name:      "unable to find sh",
			expansion: "!git branch --show-current",
			args:      []string{},
			findSh: func() (string, error) {
				return "", errors.New("unable to locate sh")
			},
			wantErr: "unable to locate sh",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotExpanded, err := expandShellAlias(tt.expansion, tt.args, tt.findSh)
			if tt.wantErr != "" {
				assert.Nil(t, gotExpanded)
				assert.EqualError(t, err, tt.wantErr)
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantExpanded, gotExpanded)
		})
	}
}

func TestNextAliasExpansionContext(t *testing.T) {
	t.Run("decrements remaining depth and records chain", func(t *testing.T) {
		ctx, err := nextAliasExpansionContext(context.Background(), "a", "pr status")
		require.NoError(t, err)

		state, ok := ctx.Value(aliasExpansionContextKey{}).(aliasExpansionState)
		require.True(t, ok)
		assert.Equal(t, config.MaxAliasExpansionDepth-1, state.remaining)
		assert.Equal(t, []string{"a"}, state.chain)
	})

	t.Run("detects circular reference", func(t *testing.T) {
		ctx, err := nextAliasExpansionContext(context.Background(), "a", "b")
		require.NoError(t, err)
		ctx, err = nextAliasExpansionContext(ctx, "b", "a")
		require.NoError(t, err)

		_, err = nextAliasExpansionContext(ctx, "a", "b")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "circular reference chain a -> b -> a")
		assert.Contains(t, err.Error(), `gh alias delete "a"`)
	})

	t.Run("detects expansion depth exhaustion", func(t *testing.T) {
		// State right at the boundary: the maximum number of expansions have
		// already been followed, so the next alias must be rejected.
		state := aliasExpansionState{
			remaining: 0,
			chain:     depthNames(config.MaxAliasExpansionDepth),
		}
		ctx := context.WithValue(context.Background(), aliasExpansionContextKey{}, state)

		_, err := nextAliasExpansionContext(ctx, "deep", "pr status")
		require.Error(t, err)
		assert.Contains(t, err.Error(), "expansion depth 11 exceeds the maximum depth 10")
		assert.Contains(t, err.Error(), "simplify the alias chain")
	})
}

func TestNewCmdBrokenAlias(t *testing.T) {
	ios, _, _, _ := iostreams.Test()
	issue := gh.AliasIssue{
		Name:  "repo x",
		Kind:  gh.AliasSelfReference,
		Chain: []string{"repo x", "repo x"},
	}
	cmd := NewCmdBrokenAlias(ios, "x", issue, "repo x")

	err := cmd.RunE(cmd, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), `alias "repo x" is self-referential`)
	assert.Contains(t, err.Error(), `gh alias delete "repo x"`)
}

// depthNames returns count zero-padded alias names a00, a01, and so on.
func depthNames(count int) []string {
	names := make([]string, 0, count)
	for i := range count {
		names = append(names, fmt.Sprintf("a%02d", i))
	}
	return names
}
