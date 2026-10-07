package doctor

import (
	"fmt"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
)

type DoctorOptions struct {
	IO     *iostreams.IOStreams
	Config gh.Config
}

func NewCmdConfigDoctor(f *cmdutil.Factory, runF func(*DoctorOptions) error) *cobra.Command {
	opts := &DoctorOptions{
		IO: f.IOStreams,
	}

	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check configuration for problems",
		Long: heredoc.Doc(`
			Check the configuration for unknown keys, invalid values, host scope
			mismatches, and self-referential, circular, or overly deep aliases.

			Both the user configuration and the built-in defaults are read so
			that stored values are judged against the current key definitions.
			Each problem reports its location, key or alias, current value,
			expected value, and whether the value came from the user
			configuration or the built-in defaults.

			The exit status is zero when no problems are found and non-zero
			otherwise.
		`),
		Example: heredoc.Doc(`
			$ gh config doctor
		`),
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := f.Config()
			if err != nil {
				return err
			}
			opts.Config = cfg

			if runF != nil {
				return runF(opts)
			}

			return doctorRun(opts)
		},
	}

	return cmd
}

func doctorRun(opts *DoctorOptions) error {
	cs := opts.IO.ColorScheme()

	issues, err := config.Check(opts.Config)
	if err != nil {
		return err
	}

	if len(issues) == 0 {
		fmt.Fprintf(opts.IO.Out, "%s No configuration problems found.\n", cs.SuccessIcon())
		return nil
	}

	for _, issue := range issues {
		fmt.Fprintf(opts.IO.Out, "%s %s\n", cs.FailureIcon(), config.FormatIssue(issue))
	}

	return cmdutil.SilentError
}
