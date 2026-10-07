// Package resolve implements a read-only view that explains where gh finds
// credentials for a host or account and which stored credential wins.
package resolve

import (
	"fmt"
	"slices"
	"strings"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/spf13/cobra"
)

// credentialResolver is implemented by the concrete auth configuration. It is
// kept local so the read-only resolution capability does not need to widen the
// gh.AuthConfig interface.
type credentialResolver interface {
	ResolveCredentials(hostname string) config.HostCredentialView
}

type resolveOptions struct {
	IO       *iostreams.IOStreams
	Config   func() (gh.Config, error)
	Exporter cmdutil.Exporter

	Hostname string
	Username string
}

// NewCmdResolve creates the `gh auth resolve` command.
func NewCmdResolve(f *cmdutil.Factory, runF func(*resolveOptions) error) *cobra.Command {
	opts := &resolveOptions{
		IO:     f.IOStreams,
		Config: f.Config,
	}

	cmd := &cobra.Command{
		Use:   "resolve",
		Args:  cobra.ExactArgs(0),
		Short: "Show where credentials for a host or account are found and which one is used",
		Long: heredoc.Docf(`
			Resolve which stored credential gh uses for a known GitHub host or account,
			without reading or printing any token content.

			For every known host (or only the one given via %[1]s--hostname%[1]s), the output
			lists stored accounts, every location that holds a credential for an account,
			the complete order in which locations are tried, and the single location that
			is selected. Each other location reports why it is not used: the value is
			empty, the location is absent or inaccessible, or an earlier location covered
			it. It also reports accounts whose stored credential is covered for commands by
			an environment variable and accounts that store credentials with different
			content in more than one place.

			The selected credential is cross-checked against %[1]sgh auth token%[1]s. A
			disagreement is reported as a conflict.

			The command is strictly read-only: it never writes configuration, creates or
			deletes secure storage entries, or changes the active account. Only token
			lengths and recognized token type prefixes are reported.
		`, "`"),
		Example: heredoc.Doc(`
			# Show credential resolution for all known hosts
			$ gh auth resolve

			# Show credential resolution for one host
			$ gh auth resolve --hostname github.example.com

			# Show credential resolution for one account
			$ gh auth resolve --hostname github.example.com --user monalisa

			# Output machine-readable JSON
			$ gh auth resolve --json hosts
		`),
		RunE: func(cmd *cobra.Command, args []string) error {
			if runF != nil {
				return runF(opts)
			}
			return resolveRun(opts)
		},
	}

	cmd.Flags().StringVarP(&opts.Hostname, "hostname", "h", "", "Resolve credentials for a specific hostname only")
	cmd.Flags().StringVarP(&opts.Username, "user", "u", "", "Resolve credentials for a specific account only")

	cmdutil.AddJSONFlags(cmd, &opts.Exporter, resolveFields)

	return cmd
}

type resolveReport struct {
	Hosts []config.HostCredentialView `json:"hosts"`
}

var resolveFields = []string{"hosts"}

// ExportData implements cmdutil exportable output.
func (r resolveReport) ExportData(fields []string) map[string]any {
	return cmdutil.StructExportData(r, fields)
}

func resolveRun(opts *resolveOptions) error {
	cfg, err := opts.Config()
	if err != nil {
		return err
	}
	authCfg := cfg.Authentication()

	resolver, ok := authCfg.(credentialResolver)
	if !ok {
		return fmt.Errorf("credential resolution is not supported by this configuration")
	}

	knownHosts := authCfg.Hosts()

	var hostnames []string
	if opts.Hostname != "" {
		if !slices.Contains(knownHosts, opts.Hostname) {
			return fmt.Errorf("unknown host %q: gh has no credentials configured for it", opts.Hostname)
		}
		hostnames = []string{opts.Hostname}
	} else {
		hostnames = slices.Clone(knownHosts)
		slices.Sort(hostnames)
	}

	// Validate the account before doing any work.
	if opts.Username != "" {
		matched := false
		for _, host := range hostnames {
			if slices.Contains(authCfg.UsersForHost(host), opts.Username) {
				matched = true
				break
			}
		}
		if !matched {
			if opts.Hostname != "" {
				return fmt.Errorf("unknown account %q on host %q", opts.Username, opts.Hostname)
			}
			return fmt.Errorf("unknown account %q: no known host stores credentials for it", opts.Username)
		}
	}

	report := resolveReport{Hosts: []config.HostCredentialView{}}
	if len(hostnames) == 0 {
		return opts.write(report, "No credentials are stored for any host known to gh.\n")
	}

	for _, hostname := range hostnames {
		view := resolver.ResolveCredentials(hostname)
		if opts.Username != "" {
			accounts := make([]config.AccountCredentialView, 0, 1)
			for _, account := range view.Accounts {
				if account.Login == opts.Username {
					accounts = append(accounts, account)
				}
			}
			view.Accounts = accounts
		}
		report.Hosts = append(report.Hosts, view)
	}

	return opts.write(report, "")
}

// write emits machine-readable output when an exporter is set and the rendered
// text report otherwise. The empty-result note is only used for human-readable
// output: machine output always carries the (empty) structured report.
func (o *resolveOptions) write(report resolveReport, emptyNote string) error {
	if o.Exporter != nil {
		return o.Exporter.Write(o.IO, report)
	}

	if emptyNote != "" && len(report.Hosts) == 0 {
		fmt.Fprint(o.IO.Out, emptyNote)
		return nil
	}

	fmt.Fprint(o.IO.Out, renderReport(o.IO.ColorScheme(), report))
	return nil
}

func renderReport(cs *iostreams.ColorScheme, report resolveReport) string {
	var sb strings.Builder
	for i, host := range report.Hosts {
		if i > 0 {
			sb.WriteString("\n")
		}
		renderHost(cs, &sb, host)
	}
	return sb.String()
}

func renderHost(cs *iostreams.ColorScheme, sb *strings.Builder, host config.HostCredentialView) {
	fmt.Fprintf(sb, "%s\n", cs.Bold(host.Hostname))

	if host.ActiveUser != "" {
		fmt.Fprintf(sb, "  Active account: %s\n", cs.Bold(host.ActiveUser))
	} else {
		sb.WriteString("  Active account: none configured\n")
	}

	if host.ActiveStep > 0 {
		source := host.Sources[host.ActiveStep-1]
		fmt.Fprintf(sb, "  Active credential: step %d, %s (%s)\n",
			source.Step, source.Label(), fingerprintText(host.ActiveFingerprint))
	} else {
		sb.WriteString("  Active credential: none; no token is currently selected for this host\n")
	}

	sb.WriteString("  Credential resolution order:\n")
	for _, source := range host.Sources {
		renderSource(sb, "    ", source)
	}

	if len(host.Accounts) > 0 {
		sb.WriteString("  Accounts:\n")
		for _, account := range host.Accounts {
			renderAccount(cs, sb, host, account)
		}
	} else {
		sb.WriteString("  Accounts: none stored for this host\n")
	}

	renderConflicts(cs, sb, "  ", host.Conflicts)
}

func renderAccount(cs *iostreams.ColorScheme, sb *strings.Builder, host config.HostCredentialView, account config.AccountCredentialView) {
	active := ""
	if account.Active {
		active = " (active)"
	}
	fmt.Fprintf(sb, "    %s%s\n", cs.Bold(account.Login), active)

	if account.EffectiveStep > 0 {
		source := account.Sources[account.EffectiveStep-1]
		fmt.Fprintf(sb,
			"      Effective credential for this account (gh auth token --hostname %s --user %s): step %d, %s (%s)\n",
			host.Hostname, account.Login, source.Step, source.Label(), fingerprintText(source.Fingerprint))
	} else {
		fmt.Fprintf(sb,
			"      Effective credential for this account: none; gh auth token --hostname %s --user %s returns no token\n",
			host.Hostname, account.Login)
	}

	sb.WriteString("      Stored credential locations:\n")
	for _, source := range account.Sources {
		renderSource(sb, "        ", source)
	}

	if account.Active {
		switch {
		case account.CommandsUseOwnCredential:
			source := host.Sources[account.CommandsCredentialStep-1]
			fmt.Fprintf(sb, "      Commands targeting %s use this account's stored credential at host resolution step %d (%s).\n",
				host.Hostname, source.Step, source.Label())
		case account.CommandsCredentialStep > 0:
			source := host.Sources[account.CommandsCredentialStep-1]
			fmt.Fprintf(sb, "      Commands targeting %s use host resolution step %d, %s, instead of a credential stored for this account.\n",
				host.Hostname, source.Step, source.Label())
		default:
			fmt.Fprintf(sb, "      Commands targeting %s currently have no selected credential.\n", host.Hostname)
		}
	}

	renderConflicts(cs, sb, "      ", account.Conflicts)
}

func renderSource(sb *strings.Builder, indent string, source config.CredentialSource) {
	fmt.Fprintf(sb, "%s%d. %s\n", indent, source.Step, source.Label())
	fmt.Fprintf(sb, "%s   state: %s", indent, source.State)
	if source.Fingerprint.Length > 0 {
		fmt.Fprintf(sb, "; %s", fingerprintText(source.Fingerprint))
	}
	switch {
	case source.AccessError != "":
		fmt.Fprintf(sb, "; %s", source.AccessError)
		if source.Reason != "" {
			fmt.Fprintf(sb, "; %s", source.Reason)
		}
	case source.Reason != "":
		fmt.Fprintf(sb, "; %s", source.Reason)
	}
	sb.WriteString("\n")
}

func renderConflicts(cs *iostreams.ColorScheme, sb *strings.Builder, indent string, conflicts []config.CredentialConflict) {
	for _, conflict := range conflicts {
		fmt.Fprintf(sb, "%s%s conflict [%s]: %s\n", indent, cs.WarningIcon(), conflict.Type, conflict.Message)
	}
}

func fingerprintText(fingerprint config.CredentialFingerprint) string {
	if fingerprint.Prefix != "" {
		return fmt.Sprintf("prefix %q, %d characters", fingerprint.Prefix, fingerprint.Length)
	}
	if fingerprint.Length > 0 {
		return fmt.Sprintf("unrecognized prefix, %d characters", fingerprint.Length)
	}
	return "empty value"
}
