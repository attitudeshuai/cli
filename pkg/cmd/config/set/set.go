package set

import (
	"errors"
	"fmt"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
)

type SetOptions struct {
	IO     *iostreams.IOStreams
	Config gh.Config

	Key      string
	Value    string
	Hostname string
}

func NewCmdConfigSet(f *cmdutil.Factory, runF func(*SetOptions) error) *cobra.Command {
	opts := &SetOptions{
		IO: f.IOStreams,
	}

	cmd := &cobra.Command{
		Use:   "set <key> <value>",
		Short: "Update configuration with a value for the given key",
		Example: heredoc.Doc(`
			$ gh config set editor vim
			$ gh config set editor "code --wait"
			$ gh config set git_protocol ssh --host github.com
			$ gh config set api_host api-gateway.example.com --host example.com
			$ gh config set prompt disabled
		`),
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			config, err := f.Config()
			if err != nil {
				return err
			}
			opts.Config = config
			opts.Key = args[0]
			opts.Value = args[1]

			if runF != nil {
				return runF(opts)
			}

			return setRun(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.Hostname, "host", "h", "", "Set per-host setting")

	return cmd
}

func setRun(opts *SetOptions) error {
	if err := config.ValidateScope(opts.Key, opts.Hostname); err != nil {
		return cmdutil.FlagErrorf("%s", err)
	}

	if err := config.ValidateKey(opts.Key); err != nil {
		warningIcon := opts.IO.ColorScheme().WarningIcon()
		fmt.Fprintf(opts.IO.ErrOut, "%s warning: '%s' is not a known configuration key\n", warningIcon, opts.Key)
	}

	if err := config.ValidateValue(opts.Key, opts.Value); err != nil {
		if invalidValue, ok := errors.AsType[config.InvalidValueError](err); ok {
			var values []string
			for _, v := range invalidValue.ValidValues {
				values = append(values, fmt.Sprintf("'%s'", v))
			}
			return fmt.Errorf("failed to set %q to %q: valid values are %v", opts.Key, opts.Value, strings.Join(values, ", "))
		}
	}

	opts.Config.Set(opts.Hostname, opts.Key, opts.Value)

	err := opts.Config.Write()
	if err != nil {
		return fmt.Errorf("failed to write config to disk: %w", err)
	}
	return nil
}

// These thin wrappers preserve the package's validation surface for its tests.
// The implementations live in internal/config so the configuration doctor and
// the configuration loading path can share the same definitions.
func ValidateKey(key string) error {
	return config.ValidateKey(key)
}

func validateScope(key, hostname string) error {
	return config.ValidateScope(key, hostname)
}

func ValidateValue(key, value string) error {
	return config.ValidateValue(key, value)
}
