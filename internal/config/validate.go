package config

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/cli/cli/v2/internal/gh"
	ghConfig "github.com/cli/go-gh/v2/pkg/config"
)

// OptionForKey returns the ConfigOption registered for key and whether key is known.
func OptionForKey(key string) (ConfigOption, bool) {
	for _, co := range Options {
		if co.Key == key {
			return co, true
		}
	}
	return ConfigOption{}, false
}

// ValidateKey returns an error when key is not a supported configuration key.
func ValidateKey(key string) error {
	if _, ok := OptionForKey(key); ok {
		return nil
	}
	return fmt.Errorf("invalid key")
}

// InvalidValueError reports that a configuration value is not one of the allowed values.
type InvalidValueError struct {
	ValidValues []string
}

func (e InvalidValueError) Error() string {
	return "invalid value"
}

// ValidateValue returns an InvalidValueError when value is not among the key's
// allowed values. Keys without an allowed-values set accept any string.
func ValidateValue(key, value string) error {
	co, ok := OptionForKey(key)
	if !ok || co.AllowedValues == nil {
		return nil
	}
	if slices.Contains(co.AllowedValues, value) {
		return nil
	}
	return InvalidValueError{ValidValues: slices.Clone(co.AllowedValues)}
}

// ValidateScope returns an error when key cannot be set for the given hostname
// scope. Unknown keys are accepted, matching the behavior of the set command.
func ValidateScope(key, hostname string) error {
	co, ok := OptionForKey(key)
	if !ok {
		return nil
	}
	switch {
	case co.Scope == ConfigScopeHostOnly && hostname == "":
		return fmt.Errorf("--host required when setting %s", key)
	case co.Scope == ConfigScopeGlobalOnly && hostname != "":
		return fmt.Errorf("--host cannot be used when setting %s", key)
	default:
		return nil
	}
}

// IssueKind categorizes a configuration health problem.
type IssueKind string

const (
	IssueUnknownKey         IssueKind = "unknown_key"
	IssueInvalidValue       IssueKind = "invalid_value"
	IssueGlobalOnlyInHost   IssueKind = "global_only_in_host"
	IssueHostOnlyInGlobal   IssueKind = "host_only_in_global"
	IssueAliasSelfReference IssueKind = "alias_self_reference"
	IssueAliasCircular      IssueKind = "alias_circular_reference"
	IssueAliasDepth         IssueKind = "alias_expansion_depth_exceeded"
)

// Issue describes one configuration health problem.
type Issue struct {
	// Kind is the problem category.
	Kind IssueKind
	// Location is where the problem was found, e.g. "global" or "host github.com".
	Location string
	// Key is the configuration key or alias name with the problem.
	Key string
	// CurrentValue is the value found in the configuration.
	CurrentValue string
	// ExpectedValue describes what an acceptable value would be.
	ExpectedValue string
	// Source reports whether the value came from the user configuration or built-in defaults.
	Source gh.ConfigSource

	// Chain carries the expansion path for alias problems.
	Chain []string
	// Depth carries the computed expansion depth for over-deep alias problems.
	Depth int
}

type rawConfigProvider interface {
	RawConfig() *ghConfig.Config
}

// Check validates the keys, values, scopes, and alias expansions in c. It reads
// the built-in defaults through the ConfigOption registry so stored values are
// judged against the current allowed values. It returns one Issue per problem
// in a deterministic order.
func Check(c gh.Config) ([]Issue, error) {
	raw, ok := c.(rawConfigProvider)
	if !ok {
		return nil, fmt.Errorf("configuration does not support health checks")
	}

	globalIssues, hostIssues := checkKeys(raw.RawConfig())
	globalIssues, hostIssues = checkDefaultValues(c, globalIssues, hostIssues)
	aliasIssues := checkAliases(c)

	issues := append(globalIssues, hostIssues...)
	return append(issues, aliasIssues...), nil
}

func checkKeys(raw *ghConfig.Config) (global, hosts []Issue) {
	topKeys, err := raw.Keys(nil)
	if err != nil {
		return nil, nil
	}
	slices.Sort(topKeys)

	for _, key := range topKeys {
		switch key {
		case hostsKey:
			hosts = checkHosts(raw)
			continue
		case aliasesKey:
			continue
		case versionKey:
			continue
		}

		co, known := OptionForKey(key)
		value := readString(raw, []string{key})
		switch {
		case !known:
			global = append(global, Issue{
				Kind:          IssueUnknownKey,
				Location:      "global",
				Key:           key,
				CurrentValue:  value,
				ExpectedValue: "a known configuration key (run 'gh config' to list supported keys)",
				Source:        gh.ConfigUserProvided,
			})
		case co.Scope == ConfigScopeHostOnly:
			global = append(global, Issue{
				Kind:          IssueHostOnlyInGlobal,
				Location:      "global",
				Key:           key,
				CurrentValue:  value,
				ExpectedValue: "the key to be set for a host with --host",
				Source:        gh.ConfigUserProvided,
			})
		default:
			if issue, ok := valueIssue("global", co, value, gh.ConfigUserProvided); ok {
				global = append(global, issue)
			}
		}
	}
	return global, hosts
}

func checkHosts(raw *ghConfig.Config) []Issue {
	hostnames, err := raw.Keys([]string{hostsKey})
	if err != nil {
		return nil
	}
	slices.Sort(hostnames)

	var issues []Issue
	for _, hostname := range hostnames {
		location := fmt.Sprintf("host %s", hostname)
		keys, err := raw.Keys([]string{hostsKey, hostname})
		if err != nil {
			continue
		}
		slices.Sort(keys)

		for _, key := range keys {
			switch key {
			case oauthTokenKey, userKey:
				continue
			case usersKey:
				issues = append(issues, checkUsers(raw, hostname)...)
				continue
			}

			co, known := OptionForKey(key)
			value := readString(raw, []string{hostsKey, hostname, key})
			switch {
			case !known:
				issues = append(issues, Issue{
					Kind:          IssueUnknownKey,
					Location:      location,
					Key:           key,
					CurrentValue:  value,
					ExpectedValue: "a known configuration key (run 'gh config' to list supported keys)",
					Source:        gh.ConfigUserProvided,
				})
			case co.Scope == ConfigScopeGlobalOnly:
				issues = append(issues, Issue{
					Kind:          IssueGlobalOnlyInHost,
					Location:      location,
					Key:           key,
					CurrentValue:  value,
					ExpectedValue: "the key to be unset for this host; it can only be set globally",
					Source:        gh.ConfigUserProvided,
				})
			default:
				if issue, ok := valueIssue(location, co, value, gh.ConfigUserProvided); ok {
					issues = append(issues, issue)
				}
			}
		}
	}
	return issues
}

func checkUsers(raw *ghConfig.Config, hostname string) []Issue {
	users, err := raw.Keys([]string{hostsKey, hostname, usersKey})
	if err != nil {
		return nil
	}
	slices.Sort(users)
	location := fmt.Sprintf("host %s", hostname)

	var issues []Issue
	for _, user := range users {
		keys, err := raw.Keys([]string{hostsKey, hostname, usersKey, user})
		if err != nil {
			continue
		}
		slices.Sort(keys)
		for _, key := range keys {
			if key == oauthTokenKey {
				continue
			}
			value := readString(raw, []string{hostsKey, hostname, usersKey, user, key})
			issues = append(issues, Issue{
				Kind:          IssueUnknownKey,
				Location:      location,
				Key:           key,
				CurrentValue:  value,
				ExpectedValue: "a known configuration key (run 'gh config' to list supported keys)",
				Source:        gh.ConfigUserProvided,
			})
		}
	}
	return issues
}

func checkDefaultValues(c gh.Config, globalIssues, hostIssues []Issue) ([]Issue, []Issue) {
	seen := map[string]bool{}
	for _, issue := range slices.Concat(globalIssues, hostIssues) {
		seen[issue.Location+"\x00"+issue.Key] = true
	}

	for _, co := range Options {
		entry, ok := c.GetOrDefault("", co.Key).Value()
		if !ok || entry.Source != gh.ConfigDefaultProvided {
			continue
		}
		issue, hasIssue := valueIssue("global", co, entry.Value, gh.ConfigDefaultProvided)
		dedupKey := "global\x00" + co.Key
		if !hasIssue || seen[dedupKey] {
			continue
		}
		globalIssues = append(globalIssues, issue)
		seen[dedupKey] = true
	}

	slices.SortStableFunc(globalIssues, func(a, b Issue) int {
		return cmp.Compare(a.Key, b.Key)
	})
	return globalIssues, hostIssues
}

func checkAliases(c gh.Config) []Issue {
	aliasCfg := c.Aliases()
	analysis := aliasCfg.Analyze()
	all := aliasCfg.All()

	issues := make([]Issue, 0, len(analysis.Issues))
	for _, a := range analysis.Issues {
		issues = append(issues, Issue{
			Kind:          aliasIssueKind(a.Kind),
			Location:      fmt.Sprintf("alias %s", a.Name),
			Key:           a.Name,
			CurrentValue:  all[a.Name],
			ExpectedValue: aliasExpected(a),
			Source:        gh.ConfigUserProvided,
			Chain:         a.Chain,
			Depth:         a.Depth,
		})
	}
	return issues
}

func valueIssue(location string, co ConfigOption, value string, source gh.ConfigSource) (Issue, bool) {
	if err := ValidateValue(co.Key, value); err != nil {
		if invalid, ok := errors.AsType[InvalidValueError](err); ok {
			return Issue{
				Kind:          IssueInvalidValue,
				Location:      location,
				Key:           co.Key,
				CurrentValue:  value,
				ExpectedValue: "one of " + strings.Join(invalid.ValidValues, ", "),
				Source:        source,
			}, true
		}
	}
	return Issue{}, false
}

func aliasIssueKind(kind gh.AliasIssueKind) IssueKind {
	switch kind {
	case gh.AliasSelfReference:
		return IssueAliasSelfReference
	case gh.AliasCircularReference:
		return IssueAliasCircular
	default:
		return IssueAliasDepth
	}
}

func aliasExpected(issue gh.AliasIssue) string {
	switch issue.Kind {
	case gh.AliasSelfReference:
		return fmt.Sprintf("an expansion that does not return to %q", issue.Name)
	case gh.AliasCircularReference:
		return "the chain to end at a gh command"
	default:
		return fmt.Sprintf("an expansion depth of at most %d", MaxAliasExpansionDepth)
	}
}

// FormatIssue returns the complete, human-readable report line for issue.
func FormatIssue(issue Issue) string {
	switch issue.Kind {
	case IssueUnknownKey:
		return fmt.Sprintf("%s: unknown key %q with value %q; expected %s; value from %s",
			issue.Location, issue.Key, issue.CurrentValue, issue.ExpectedValue, sourceLabel(issue.Source))
	case IssueInvalidValue:
		return fmt.Sprintf("%s: key %q has invalid value %q; expected %s; value from %s",
			issue.Location, issue.Key, issue.CurrentValue, issue.ExpectedValue, sourceLabel(issue.Source))
	case IssueGlobalOnlyInHost:
		return fmt.Sprintf("%s: key %q can only be set globally but is set for the host; current value %q; expected %s; value from %s",
			issue.Location, issue.Key, issue.CurrentValue, issue.ExpectedValue, sourceLabel(issue.Source))
	case IssueHostOnlyInGlobal:
		return fmt.Sprintf("%s: key %q requires a host but is set globally; current value %q; expected %s; value from %s",
			issue.Location, issue.Key, issue.CurrentValue, issue.ExpectedValue, sourceLabel(issue.Source))
	case IssueAliasSelfReference:
		return formatAliasIssue(issue, gh.AliasSelfReference)
	case IssueAliasCircular:
		return formatAliasIssue(issue, gh.AliasCircularReference)
	case IssueAliasDepth:
		return formatAliasIssue(issue, gh.AliasExpansionDepthExceeded)
	}
	return ""
}

func formatAliasIssue(issue Issue, kind gh.AliasIssueKind) string {
	analysisIssue := gh.AliasIssue{
		Name:  issue.Key,
		Kind:  kind,
		Chain: issue.Chain,
		Depth: issue.Depth,
	}
	return fmt.Sprintf("%s; value from %s",
		AliasIssueMessage(analysisIssue, issue.CurrentValue),
		sourceLabel(issue.Source))
}

func sourceLabel(source gh.ConfigSource) string {
	switch source {
	case gh.ConfigDefaultProvided:
		return "built-in defaults"
	default:
		return "user configuration"
	}
}

// AliasIssueMessage returns a human-readable explanation of issue, including the
// current expansion value. It is shared by the configuration doctor and the
// runtime alias expansion guard.
func AliasIssueMessage(issue gh.AliasIssue, value string) string {
	switch issue.Kind {
	case gh.AliasSelfReference:
		return fmt.Sprintf("alias %q is self-referential: expansion %q returns to the alias; expected an expansion that does not return to %q",
			issue.Name, value, issue.Name)
	case gh.AliasCircularReference:
		return fmt.Sprintf("alias %q is in a circular reference chain %s; expected the chain to end at a gh command",
			issue.Name, strings.Join(issue.Chain, " -> "))
	default:
		return fmt.Sprintf("alias %q expansion depth %d exceeds the maximum depth %d; expected an expansion depth of at most %d",
			issue.Name, issue.Depth, MaxAliasExpansionDepth, MaxAliasExpansionDepth)
	}
}

// AliasIssueSuggestion returns how to resolve issue.
func AliasIssueSuggestion(issue gh.AliasIssue) string {
	switch issue.Kind {
	case gh.AliasExpansionDepthExceeded:
		return fmt.Sprintf("simplify the alias chain, or redefine it with 'gh alias set %q <expansion>'", issue.Name)
	default:
		return fmt.Sprintf("run 'gh alias delete %q' or redefine it with 'gh alias set %q <expansion>'", issue.Name, issue.Name)
	}
}

func readString(raw *ghConfig.Config, keys []string) string {
	value, err := raw.Get(keys)
	if err != nil {
		return ""
	}
	return value
}
