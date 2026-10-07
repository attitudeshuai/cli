package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/internal/keyring"
	ghauth "github.com/cli/go-gh/v2/pkg/auth"
)

// CredentialSourceClass reports the kind of location a credential was found in.
type CredentialSourceClass string

const (
	// CredentialFromEnvironment is an environment variable such as GH_TOKEN.
	CredentialFromEnvironment CredentialSourceClass = "environment_variable"
	// CredentialFromConfigFile is an oauth_token entry in the hosts config file.
	CredentialFromConfigFile CredentialSourceClass = "config_file"
	// CredentialFromSecureStorage is an entry in the operating system credential store.
	CredentialFromSecureStorage CredentialSourceClass = "secure_storage"
)

// CredentialSourceState reports what happened when a credential location was
// examined during resolution.
type CredentialSourceState string

const (
	// SourceSelected is the location whose credential resolution actually uses.
	SourceSelected CredentialSourceState = "selected"
	// SourceShadowed is a location that holds a credential but loses to a
	// higher-priority location.
	SourceShadowed CredentialSourceState = "shadowed"
	// SourceEmpty is a location that is present but whose value is empty.
	SourceEmpty CredentialSourceState = "empty"
	// SourceInaccessible is a location that could not be read, for example an
	// unreachable keyring. ErrNotFound is reported as SourceAbsent instead.
	SourceInaccessible CredentialSourceState = "inaccessible"
	// SourceAbsent is a location where no credential is stored.
	SourceAbsent CredentialSourceState = "absent"
	// SourceUnavailable is a location resolution never reaches, for example the
	// active user keyring slot when the host has no active user configured.
	SourceUnavailable CredentialSourceState = "unavailable"
)

// Conflict types reported by the credential resolution view.
const (
	// ConflictDuplicateCredentials means one account stores credentials with
	// different content in more than one location. The winner is deterministic
	// by precedence.
	ConflictDuplicateCredentials = "duplicate_stored_credentials"
	// ConflictActiveCredentialMismatch means the active account's own credential
	// differs from the one command resolution selects for the host.
	ConflictActiveCredentialMismatch = "active_credential_mismatch"
	// ConflictResolutionInconsistency means the view disagrees with the token
	// returned by the existing resolution entry points. It should be impossible
	// and indicates a bug in the view.
	ConflictResolutionInconsistency = "resolution_inconsistency"
)

// Environment variables consulted by go-gh when resolving a token, in the
// order they are tried. The names mirror github.com/cli/go-gh/v2/pkg/auth.
var (
	githubTokenEnvVars     = []string{"GH_TOKEN", "GITHUB_TOKEN"}
	enterpriseTokenEnvVars = []string{"GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
)

// CredentialFingerprint identifies a credential without exposing it: only its
// length and its recognizable type prefix are ever reported.
type CredentialFingerprint struct {
	// Length is the number of characters in the credential.
	Length int `json:"length"`
	// Prefix is the recognized GitHub token type prefix, or the empty string for
	// credentials gh does not recognize.
	Prefix string `json:"prefix"`
}

// CredentialSource describes one location examined while resolving a credential.
type CredentialSource struct {
	// Step is the 1-based position of this location in the resolution order.
	Step int `json:"step"`
	// Class is the category of the location.
	Class CredentialSourceClass `json:"class"`
	// Location is the concrete place the credential lives: the environment
	// variable name, the config file path, or the secure storage service name.
	Location string `json:"location"`
	// Identifier uniquely names the entry inside its location, for example the
	// config key or the secure storage account key.
	Identifier string `json:"identifier"`
	// Account is the username key of a per-user secure storage entry. The empty
	// string is the host's unkeyed legacy entry.
	Account string `json:"account,omitempty"`
	// State is the outcome of examining this location.
	State CredentialSourceState `json:"state"`
	// Reason explains why a location was not selected in human-readable terms.
	Reason string `json:"reason,omitempty"`
	// AccessError carries the read error when State is inaccessible.
	AccessError string `json:"accessError,omitempty"`
	// Fingerprint describes the stored value when one is present.
	Fingerprint CredentialFingerprint `json:"fingerprint"`

	// value holds the credential long enough to compare credentials and detect
	// conflicts. It is never serialized and must never be rendered.
	value string
	// eligible reports whether the existing resolution code would use this
	// location as a candidate given the outcome of earlier lookups.
	eligible bool
	// slot distinguishes secure storage rows that share an empty account key.
	slot secureStorageSlot
}

// secureStorageSlot identifies which secure storage lookup a source represents.
type secureStorageSlot string

const (
	// secureStorageSlotUser is a per-account keyed entry.
	secureStorageSlotUser secureStorageSlot = "user"
	// secureStorageSlotActiveUser is the entry looked up with the active user.
	secureStorageSlotActiveUser secureStorageSlot = "active_user"
	// secureStorageSlotUnkeyed is the host's unkeyed legacy entry.
	secureStorageSlotUnkeyed secureStorageSlot = "unkeyed"
)

// Label returns a stable, human-readable description of the location.
func (s CredentialSource) Label() string {
	switch s.Class {
	case CredentialFromEnvironment:
		return fmt.Sprintf("environment variable %s", s.Location)
	case CredentialFromConfigFile:
		return fmt.Sprintf("config file %s [%s]", s.Location, s.Identifier)
	case CredentialFromSecureStorage:
		switch s.slot {
		case secureStorageSlotUnkeyed:
			return fmt.Sprintf("secure storage %s, unkeyed entry", s.Location)
		case secureStorageSlotActiveUser, "":
			if s.Account == "" {
				return fmt.Sprintf("secure storage %s, active user entry", s.Location)
			}
		}
		return fmt.Sprintf("secure storage %s, account %q", s.Location, s.Account)
	default:
		return string(s.Class)
	}
}

// CredentialConflict reports a deterministic finding about credentials that
// compete with or diverge from each other. It never contains token content.
type CredentialConflict struct {
	// Type is one of the Conflict* constants.
	Type string `json:"type"`
	// Message describes the finding in human-readable terms.
	Message string `json:"message"`
	// WinnerStep is the 1-based step of the credential that takes precedence.
	WinnerStep int `json:"winnerStep,omitempty"`
	// LoserStep is the 1-based step of the credential that loses out.
	LoserStep int `json:"loserStep,omitempty"`
}

// AccountCredentialView lists every location that stores a given account's
// credential and identifies which one existing token resolution returns.
type AccountCredentialView struct {
	// Login is the account username.
	Login string `json:"login"`
	// Active reports whether this is the host's active account.
	Active bool `json:"active"`
	// Sources lists the account's own storage locations in the order
	// TokenForUser tries them: per-user secure storage, then per-user config.
	Sources []CredentialSource `json:"sources"`
	// EffectiveStep is the 1-based step of the credential returned by
	// `gh auth token --hostname HOST --user LOGIN`; zero means no credential is
	// returned.
	EffectiveStep int `json:"effectiveStep"`
	// CommandsUseOwnCredential reports whether command resolution for the host
	// uses a credential stored at one of this account's own locations. It is
	// false for inactive accounts and whenever an environment variable or a
	// different active-slot credential wins for the host.
	CommandsUseOwnCredential bool `json:"commandsUseOwnCredential"`
	// CommandsCredentialStep is the 1-based host resolution step of the
	// credential commands use when targeting the host; zero when none.
	CommandsCredentialStep int `json:"commandsCredentialStep"`
	// Conflicts reports credentials that compete with or diverge from this
	// account's effective credential.
	Conflicts []CredentialConflict `json:"conflicts"`
}

// HostCredentialView describes credential resolution for one known host.
type HostCredentialView struct {
	// Hostname is the host the view describes.
	Hostname string `json:"hostname"`
	// ActiveUser is the configured active account, or the empty string when the
	// host has none.
	ActiveUser string `json:"activeUser"`
	// ActiveSource is the source string reported by AuthConfig.ActiveToken, such
	// as GH_TOKEN, oauth_token, keyring, or default.
	ActiveSource string `json:"activeSource"`
	// ActiveFingerprint describes the token ActiveToken returns.
	ActiveFingerprint CredentialFingerprint `json:"activeFingerprint"`
	// ActiveStep is the 1-based step of the selected location in Sources; zero
	// means no credential is selected.
	ActiveStep int `json:"activeStep"`
	// Sources lists every location considered by ActiveToken, in the order it is
	// considered: applicable environment variables, the host level config token,
	// the active user's secure storage entry, and the unkeyed fallback entry.
	Sources []CredentialSource `json:"sources"`
	// Accounts lists the host's stored accounts with the active account first.
	Accounts []AccountCredentialView `json:"accounts"`
	// Conflicts reports host level findings; per-account findings live on the
	// account they concern.
	Conflicts []CredentialConflict `json:"conflicts"`
}

// ResolveCredentials produces a strictly read-only view of where credentials
// for the host are stored, which one existing resolution selects, and why each
// other location loses. It performs no writes and does not read or return any
// token content: results carry only token lengths, recognized type prefixes,
// locations, and states.
//
// The ordering mirrors AuthConfig.ActiveToken and AuthConfig.TokenForUser
// exactly; each view is cross-checked against those entry points and a
// disagreement is reported as a resolution_inconsistency conflict.
func (c *AuthConfig) ResolveCredentials(hostname string) HostCredentialView {
	normalized := ghauth.NormalizeHostname(hostname)
	service := keyringServiceName(hostname)
	hostsFile := filepath.Join(ConfigDir(), "hosts.yml")

	view := HostCredentialView{Hostname: hostname}
	if activeUser, err := c.ActiveUser(hostname); err == nil {
		view.ActiveUser = activeUser
	}

	// Host level resolution, mirroring ActiveToken.
	var sources []CredentialSource
	step := 0
	nextStep := func() int {
		step++
		return step
	}

	// First the applicable environment variables, in go-gh precedence.
	envVars := githubTokenEnvVars
	if ghauth.IsEnterprise(normalized) {
		envVars = enterpriseTokenEnvVars
	}
	for _, name := range envVars {
		row := CredentialSource{
			Step:       nextStep(),
			Class:      CredentialFromEnvironment,
			Location:   name,
			Identifier: name,
			eligible:   true,
		}
		value, present := os.LookupEnv(name)
		row.value = value
		row.State, row.Reason = probeState(value, present, nil)
		if value != "" {
			row.Fingerprint = fingerprintToken(value)
		}
		if !present {
			row.Reason = fmt.Sprintf("%s is not set", name)
		} else if value == "" {
			row.Reason = fmt.Sprintf("%s is set but empty", name)
		}
		sources = append(sources, row)
	}

	// Then the host level plain text token in the config file.
	hostConfigID := fmt.Sprintf("%s.%s.%s", hostsKey, normalized, oauthTokenKey)
	hostConfigRow := CredentialSource{
		Step:       nextStep(),
		Class:      CredentialFromConfigFile,
		Location:   hostsFile,
		Identifier: hostConfigID,
		eligible:   true,
	}
	hostToken, hostConfigErr := c.cfg.Get([]string{hostsKey, normalized, oauthTokenKey})
	hostConfigRow.value = hostToken
	hostConfigRow.State, hostConfigRow.Reason = probeState(hostToken, hostConfigErr == nil, nil)
	if hostToken != "" {
		hostConfigRow.Fingerprint = fingerprintToken(hostToken)
		hostConfigRow.Reason = ""
	} else if hostConfigErr != nil {
		hostConfigRow.Reason = fmt.Sprintf("no %s entry in %s", hostConfigID, hostsFile)
	} else {
		hostConfigRow.Reason = fmt.Sprintf("%s in %s is empty", hostConfigID, hostsFile)
	}
	sources = append(sources, hostConfigRow)

	// Then the active user's secure storage entry. ActiveToken only tries this
	// when the host has an active user.
	activeUserRow := CredentialSource{
		Step:       nextStep(),
		Class:      CredentialFromSecureStorage,
		Location:   service,
		Identifier: service,
		Account:    view.ActiveUser,
		slot:       secureStorageSlotActiveUser,
	}
	var activeUserKeyringErr error
	if view.ActiveUser != "" {
		activeUserRow.eligible = true
		var token string
		token, activeUserKeyringErr = c.TokenFromKeyringForUser(hostname, view.ActiveUser)
		activeUserRow.value = token
		activeUserRow.State, activeUserRow.Reason = probeState(token, activeUserKeyringErr == nil, activeUserKeyringErr)
		if token != "" {
			activeUserRow.Fingerprint = fingerprintToken(token)
		}
		switch {
		case activeUserKeyringErr == nil:
		case errors.Is(activeUserKeyringErr, keyring.ErrNotFound):
			activeUserRow.Reason = fmt.Sprintf("no secure storage entry for service %s account %q", service, view.ActiveUser)
		default:
			activeUserRow.Reason = ""
			activeUserRow.AccessError = activeUserKeyringErr.Error()
		}
	} else {
		activeUserRow.State = SourceUnavailable
		activeUserRow.Reason = "no active user is configured for the host, so this entry is never tried"
	}
	sources = append(sources, activeUserRow)

	// Finally the unkeyed fallback entry. ActiveToken tries this only when the
	// active user lookup failed, but the view reads the entry regardless so an
	// inaccessible store or a shadowed legacy token stays visible.
	unkeyedRow := CredentialSource{
		Step:       nextStep(),
		Class:      CredentialFromSecureStorage,
		Location:   service,
		Identifier: service,
		slot:       secureStorageSlotUnkeyed,
	}
	unkeyedToken, unkeyedErr := c.TokenFromKeyring(hostname)
	unkeyedRow.value = unkeyedToken
	unkeyedRow.State, unkeyedRow.Reason = probeState(unkeyedToken, unkeyedErr == nil, unkeyedErr)
	if unkeyedToken != "" {
		unkeyedRow.Fingerprint = fingerprintToken(unkeyedToken)
	}
	switch {
	case unkeyedErr == nil:
	case errors.Is(unkeyedErr, keyring.ErrNotFound):
		unkeyedRow.Reason = fmt.Sprintf("no unkeyed secure storage entry for service %s", service)
	default:
		unkeyedRow.Reason = ""
		unkeyedRow.AccessError = unkeyedErr.Error()
	}
	// The fallback is a resolution candidate only when the active user lookup
	// was not made or returned an error.
	unkeyedRow.eligible = view.ActiveUser == "" || activeUserKeyringErr != nil
	sources = append(sources, unkeyedRow)

	// Select the first eligible non-empty value, which is what ActiveToken does.
	winner := -1
	for i := range sources {
		if !sources[i].eligible {
			continue
		}
		if sources[i].State == SourceInaccessible || sources[i].value == "" {
			continue
		}
		winner = i
		break
	}
	for i := range sources {
		switch {
		case i == winner:
			sources[i].State = SourceSelected
			sources[i].Reason = ""
		case sources[i].value != "" && sources[i].State != SourceInaccessible:
			// probeState marks any non-empty candidate provisionally as
			// selected; anything that did not win is covered by an earlier
			// location or never consulted.
			sources[i].State = SourceShadowed
		}
		if sources[i].State == SourceShadowed {
			if winner >= 0 {
				sources[i].Reason = fmt.Sprintf("covered by step %d, %s", sources[winner].Step, sources[winner].Label())
			} else if !sources[i].eligible {
				sources[i].Reason = "not consulted because an earlier lookup succeeded"
			}
		}
	}
	view.Sources = sources
	if winner >= 0 {
		view.ActiveStep = sources[winner].Step
	}

	// Cross-check the view against ActiveToken.
	activeToken, activeSource := c.ActiveToken(hostname)
	view.ActiveSource = activeSource
	view.ActiveFingerprint = fingerprintToken(activeToken)
	switch {
	case activeToken == "" && winner < 0:
		// Agree: no credential resolves for the host.
	case activeToken == "" || winner < 0 || sources[winner].value != activeToken:
		view.Conflicts = append(view.Conflicts, CredentialConflict{
			Type:    ConflictResolutionInconsistency,
			Message: fmt.Sprintf("the resolution view disagrees with the active token selected for %s", hostname),
		})
	default:
		// Agree: the winner is the token ActiveToken returned.
	}

	// Account level resolution, mirroring TokenForUser for each stored user.
	users := c.UsersForHost(hostname)
	orderedUsers := orderUsers(users, view.ActiveUser)
	for _, user := range orderedUsers {
		view.Accounts = append(view.Accounts, c.resolveAccount(hostname, normalized, service, hostsFile, user, view))
	}

	return view
}

// resolveAccount enumerates the locations storing one account's credential and
// reconciles them with host level command resolution.
func (c *AuthConfig) resolveAccount(hostname, normalized, service, hostsFile, user string, hostView HostCredentialView) AccountCredentialView {
	account := AccountCredentialView{
		Login:  user,
		Active: user == hostView.ActiveUser && user != "",
	}

	// TokenForUser tries per-user secure storage first.
	keyringRow := CredentialSource{
		Step:       1,
		Class:      CredentialFromSecureStorage,
		Location:   service,
		Identifier: service,
		Account:    user,
		slot:       secureStorageSlotUser,
		eligible:   true,
	}
	tokenFromKeyring, keyringErr := c.TokenFromKeyringForUser(hostname, user)
	keyringRow.value = tokenFromKeyring
	keyringRow.State, keyringRow.Reason = probeState(tokenFromKeyring, keyringErr == nil, keyringErr)
	if tokenFromKeyring != "" {
		keyringRow.Fingerprint = fingerprintToken(tokenFromKeyring)
	}
	switch {
	case keyringErr == nil:
	case errors.Is(keyringErr, keyring.ErrNotFound):
		keyringRow.Reason = fmt.Sprintf("no secure storage entry for service %s account %q", service, user)
	default:
		keyringRow.Reason = ""
		keyringRow.AccessError = keyringErr.Error()
	}

	// Then the per-user plain text config entry. As in TokenForUser it is only a
	// candidate when the keyring lookup errors.
	configID := fmt.Sprintf("%s.%s.%s.%s.%s", hostsKey, normalized, usersKey, user, oauthTokenKey)
	configRow := CredentialSource{
		Step:       2,
		Class:      CredentialFromConfigFile,
		Location:   hostsFile,
		Identifier: configID,
		eligible:   keyringErr != nil,
	}
	configToken, configErr := c.cfg.Get([]string{hostsKey, normalized, usersKey, user, oauthTokenKey})
	configRow.value = configToken
	configRow.State, configRow.Reason = probeState(configToken, configErr == nil, nil)
	if configToken != "" {
		configRow.Fingerprint = fingerprintToken(configToken)
		configRow.Reason = ""
	} else if configErr != nil {
		configRow.Reason = fmt.Sprintf("no %s entry in %s", configID, hostsFile)
	} else {
		configRow.Reason = fmt.Sprintf("%s in %s is empty", configID, hostsFile)
	}

	rows := []CredentialSource{keyringRow, configRow}
	accountWinner := -1
	for i := range rows {
		if !rows[i].eligible || rows[i].State == SourceInaccessible || rows[i].value == "" {
			continue
		}
		accountWinner = i
		break
	}
	for i := range rows {
		switch {
		case i == accountWinner:
			rows[i].State = SourceSelected
			rows[i].Reason = ""
		case rows[i].value != "" && rows[i].State != SourceInaccessible:
			rows[i].State = SourceShadowed
			if accountWinner >= 0 {
				rows[i].Reason = fmt.Sprintf("covered by step %d, %s", rows[accountWinner].Step, rows[accountWinner].Label())
			} else {
				rows[i].Reason = "not consulted because an earlier lookup succeeded"
			}
		}
	}
	account.Sources = rows
	if accountWinner >= 0 {
		account.EffectiveStep = rows[accountWinner].Step
	}

	// More than one stored credential with different content is a deterministic
	// conflict: precedence decides, but the loser stays around and is easy to
	// confuse for the live credential.
	if tokenFromKeyring != "" && configToken != "" && tokenFromKeyring != configToken {
		winnerRow, loserRow := rows[0], rows[1]
		if accountWinner == 1 {
			winnerRow, loserRow = rows[1], rows[0]
		}
		account.Conflicts = append(account.Conflicts, CredentialConflict{
			Type:       ConflictDuplicateCredentials,
			WinnerStep: winnerRow.Step,
			LoserStep:  loserRow.Step,
			Message: fmt.Sprintf(
				"account %q on %s stores different credentials in %s and %s; step %d wins deterministically and is returned by gh auth token",
				user, hostname, loserRow.Label(), winnerRow.Label(), winnerRow.Step),
		})
	}

	// Cross-check the view against TokenForUser.
	tokenForUser, _, tokenForUserErr := c.TokenForUser(hostname, user)
	switch {
	case accountWinner < 0 && (tokenForUserErr != nil || tokenForUser == ""):
		// Agree: no usable credential is returned for the account.
	case accountWinner >= 0 && tokenForUserErr == nil && rows[accountWinner].value == tokenForUser:
		// Agree with the official entry point.
	default:
		account.Conflicts = append(account.Conflicts, CredentialConflict{
			Type:    ConflictResolutionInconsistency,
			Message: fmt.Sprintf("the resolution view disagrees with the token returned for %s account %q", hostname, user),
		})
	}

	// Reconcile the account's own credential with what commands actually use.
	if account.Active && hostView.ActiveStep > 0 {
		account.CommandsCredentialStep = hostView.ActiveStep
		hostSource := hostView.Sources[hostView.ActiveStep-1]
		switch {
		case hostSource.Class == CredentialFromEnvironment:
			account.CommandsUseOwnCredential = false
		case accountWinner >= 0 && rows[accountWinner].value == hostSource.value:
			account.CommandsUseOwnCredential = true
		case accountWinner >= 0 && rows[accountWinner].value != "" && hostSource.value != "":
			account.CommandsUseOwnCredential = false
			account.Conflicts = append(account.Conflicts, CredentialConflict{
				Type:       ConflictActiveCredentialMismatch,
				WinnerStep: hostSource.Step,
				LoserStep:  rows[accountWinner].Step,
				Message: fmt.Sprintf(
					"commands targeting %s use the credential at host resolution step %d (%s), but gh auth token --hostname %s --user %s returns a different credential at account step %d (%s); host resolution wins deterministically",
					hostname, hostSource.Step, hostSource.Label(), hostname, user, rows[accountWinner].Step, rows[accountWinner].Label()),
			})
		}
	}

	return account
}

// probeState classifies a lookup without knowing the storage details.
func probeState(value string, present bool, err error) (CredentialSourceState, string) {
	switch {
	case err != nil && !errors.Is(err, keyring.ErrNotFound):
		return SourceInaccessible, err.Error()
	case !present:
		return SourceAbsent, ""
	case value == "":
		return SourceEmpty, ""
	default:
		return SourceSelected, ""
	}
}

// orderUsers lists the active user first and every other stored user in stable
// lexical order.
func orderUsers(users []string, activeUser string) []string {
	others := make([]string, 0, len(users))
	for _, user := range users {
		if user != activeUser {
			others = append(others, user)
		}
	}
	slices.Sort(others)
	ordered := make([]string, 0, len(users))
	if activeUser != "" && slices.Contains(users, activeUser) {
		ordered = append(ordered, activeUser)
	}
	return append(ordered, others...)
}

// fingerprintToken returns the safe fingerprint of a credential: its length and
// recognizable type prefix only. The prefix values come from gh.TokenTypes.
func fingerprintToken(token string) CredentialFingerprint {
	fingerprint := CredentialFingerprint{Length: len(token)}
	for _, tokenType := range gh.TokenTypes {
		if strings.HasPrefix(token, string(tokenType)) {
			fingerprint.Prefix = string(tokenType)
			break
		}
	}
	return fingerprint
}
