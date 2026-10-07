package root

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/internal/run"
	"github.com/cli/cli/v2/internal/text"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/findsh"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/google/shlex"
	"github.com/spf13/cobra"
)

func NewCmdShellAlias(io *iostreams.IOStreams, aliasName, aliasValue string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   aliasName,
		Short: fmt.Sprintf("Shell alias for %q", text.Truncate(80, aliasValue)),
		RunE: func(c *cobra.Command, args []string) error {
			expandedArgs, err := expandShellAlias(aliasValue, args, nil)
			if err != nil {
				return err
			}
			externalCmd := exec.Command(expandedArgs[0], expandedArgs[1:]...)
			externalCmd.Stderr = io.ErrOut
			externalCmd.Stdout = io.Out
			externalCmd.Stdin = io.In
			preparedCmd := run.PrepareCmd(externalCmd)
			if err = preparedCmd.Run(); err != nil {
				if execError, ok := errors.AsType[*exec.ExitError](err); ok {
					return &ExternalCommandExitError{execError}
				}
				return fmt.Errorf("failed to run external command: %w\n", err)
			}
			return nil
		},
		GroupID:            "alias",
		DisableFlagParsing: true,
	}
	cmdutil.DisableAuthCheck(cmd)
	// Aliases are user-defined names and must not be reported as telemetry
	// dimensions, since the name itself may be sensitive (e.g. project or
	// organization names).
	cmdutil.DisableTelemetry(cmd)
	return cmd
}

func NewCmdAlias(io *iostreams.IOStreams, aliasName, fullAliasName, aliasValue string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   aliasName,
		Short: fmt.Sprintf("Alias for %q", text.Truncate(80, aliasValue)),
		RunE: func(c *cobra.Command, args []string) error {
			expandedArgs, err := expandAlias(aliasValue, args)
			if err != nil {
				return err
			}
			root := c.Root()
			ctx, err := nextAliasExpansionContext(c.Context(), fullAliasName, aliasValue)
			if err != nil {
				return err
			}
			root.SetContext(ctx)
			root.SetArgs(expandedArgs)
			return root.Execute()
		},
		GroupID:            "alias",
		DisableFlagParsing: true,
		Annotations: map[string]string{
			aliasExpansionAnnotation: aliasValue,
		},
	}
	cmdutil.DisableAuthCheck(cmd)
	// Aliases are user-defined names and must not be reported as telemetry
	// dimensions, since the name itself may be sensitive (e.g. project or
	// organization names).
	cmdutil.DisableTelemetry(cmd)
	return cmd
}

// NewCmdBrokenAlias registers an alias whose expansion failed validation when
// the configuration was loaded. Invoking it reports the reason and how to fix
// the alias instead of re-entering the command tree.
func NewCmdBrokenAlias(io *iostreams.IOStreams, aliasName string, issue gh.AliasIssue, aliasValue string) *cobra.Command {
	cmd := &cobra.Command{
		Use:   aliasName,
		Short: fmt.Sprintf("Broken alias for %q", text.Truncate(80, aliasValue)),
		RunE: func(c *cobra.Command, args []string) error {
			return fmt.Errorf("%s; %s",
				config.AliasIssueMessage(issue, aliasValue),
				config.AliasIssueSuggestion(issue))
		},
		GroupID:            "alias",
		DisableFlagParsing: true,
	}
	cmdutil.DisableAuthCheck(cmd)
	cmdutil.DisableTelemetry(cmd)
	return cmd
}

// aliasExpansionState tracks how many further alias expansions may be followed
// and which aliases have already been expanded. It is carried through cobra's
// execution context so each alias re-entry can enforce the same hard bound.
type aliasExpansionState struct {
	remaining int
	chain     []string
}

type aliasExpansionContextKey struct{}

// nextAliasExpansionContext validates the next alias re-entry and returns the
// context carrying the updated state. It rejects a return to an already
// expanded alias (a circular reference) and exhausting the remaining depth
// (an overly deep chain), with the same readable messages as the doctor.
func nextAliasExpansionContext(ctx context.Context, aliasName, aliasValue string) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	state := aliasExpansionState{remaining: config.MaxAliasExpansionDepth}
	if s, ok := ctx.Value(aliasExpansionContextKey{}).(aliasExpansionState); ok {
		state = s
	}

	var issue gh.AliasIssue
	switch {
	case slices.Contains(state.chain, aliasName):
		issue = gh.AliasIssue{
			Name:  aliasName,
			Kind:  gh.AliasCircularReference,
			Chain: append(slices.Clone(state.chain), aliasName),
		}
	case state.remaining <= 0:
		issue = gh.AliasIssue{
			Name:  aliasName,
			Kind:  gh.AliasExpansionDepthExceeded,
			Depth: config.MaxAliasExpansionDepth + 1,
		}
	default:
		state.remaining--
		state.chain = append(slices.Clone(state.chain), aliasName)
		return context.WithValue(ctx, aliasExpansionContextKey{}, state), nil
	}

	return nil, fmt.Errorf("%s; %s",
		config.AliasIssueMessage(issue, aliasValue),
		config.AliasIssueSuggestion(issue))
}

// ExpandAlias processes argv to see if it should be rewritten according to a user's aliases.
func expandAlias(expansion string, args []string) ([]string, error) {
	extraArgs := []string{}
	for i, a := range args {
		if !strings.Contains(expansion, "$") {
			extraArgs = append(extraArgs, a)
		} else {
			expansion = strings.ReplaceAll(expansion, fmt.Sprintf("$%d", i+1), a)
		}
	}

	lingeringRE := regexp.MustCompile(`\$\d`)
	if lingeringRE.MatchString(expansion) {
		return nil, fmt.Errorf("not enough arguments for alias: %s", expansion)
	}

	newArgs, err := shlex.Split(expansion)
	if err != nil {
		return nil, err
	}

	expanded := append(newArgs, extraArgs...)

	return expanded, nil
}

// ExpandShellAlias processes argv to see if it should be rewritten according to a user's aliases.
func expandShellAlias(expansion string, args []string, findShFunc func() (string, error)) ([]string, error) {
	if findShFunc == nil {
		findShFunc = findSh
	}

	shPath, shErr := findShFunc()
	if shErr != nil {
		return nil, shErr
	}

	expanded := []string{shPath, "-c", expansion[1:]}

	if len(args) > 0 {
		expanded = append(expanded, "--")
		expanded = append(expanded, args...)
	}

	return expanded, nil
}

func findSh() (string, error) {
	shPath, err := findsh.Find()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			if runtime.GOOS == "windows" {
				return "", errors.New("unable to locate sh to execute the shell alias with. The sh.exe interpreter is typically distributed with Git for Windows.")
			}
			return "", errors.New("unable to locate sh to execute shell alias with")
		}
		return "", err
	}
	return shPath, nil
}
