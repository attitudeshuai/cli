package extensions

import (
	"io"

	"github.com/cli/cli/v2/internal/ghrepo"
)

type ExtTemplateType int

const (
	GitTemplateType      ExtTemplateType = 0
	GoBinTemplateType    ExtTemplateType = 1
	OtherBinTemplateType ExtTemplateType = 2
)

//go:generate moq -rm -out extension_mock.go . Extension
type Extension interface {
	Name() string // Extension Name without gh-
	Path() string // Path to executable
	URL() string
	CurrentVersion() string
	LatestVersion() string
	IsPinned() bool
	UpdateAvailable() bool
	IsBinary() bool
	IsLocal() bool
	Owner() string
}

// UpgradeOutcome categorizes the result of processing one extension during an upgrade.
type UpgradeOutcome string

const (
	// UpgradeOutcomeUpgraded indicates an extension was successfully upgraded.
	UpgradeOutcomeUpgraded UpgradeOutcome = "upgraded"
	// UpgradeOutcomeSkipped indicates an extension was intentionally not upgraded.
	// See SkipReason* constants for the skip reason.
	UpgradeOutcomeSkipped UpgradeOutcome = "skipped"
	// UpgradeOutcomeFailed indicates an extension could not be upgraded; the extension
	// has been restored to its pre-upgrade state when Message says so.
	UpgradeOutcomeFailed UpgradeOutcome = "failed"
	// UpgradeOutcomeRecovered indicates an interrupted upgrade from a previous run was
	// restored to the pre-upgrade state during startup reconciliation.
	UpgradeOutcomeRecovered UpgradeOutcome = "recovered"
	// UpgradeOutcomeIntermediate indicates an interrupted upgrade from a previous run
	// could not be restored; the extension is in an unknown intermediate state.
	UpgradeOutcomeIntermediate UpgradeOutcome = "intermediate"
)

const (
	// SkipReasonPinned is the UpgradeRecord.Reason for a skipped pinned extension.
	SkipReasonPinned = "pinned"
	// SkipReasonLocal is the UpgradeRecord.Reason for a skipped local extension.
	SkipReasonLocal = "local"
	// SkipReasonUpToDate is the UpgradeRecord.Reason for a skipped up-to-date extension.
	SkipReasonUpToDate = "up-to-date"
)

const (
	// PreflightReasonTargetVersion is the UpgradeRecord.Reason when the target version
	// cannot be retrieved during preflight.
	PreflightReasonTargetVersion = "target-version-unreachable"
	// PreflightReasonPlatformAsset is the UpgradeRecord.Reason when no installable asset
	// exists for the target platform during preflight.
	PreflightReasonPlatformAsset = "platform-asset-missing"
	// PreflightReasonTargetNotWritable is the UpgradeRecord.Reason when the target
	// directory is not writable during preflight.
	PreflightReasonTargetNotWritable = "target-not-writable"
)

// UpgradeOptions holds the opt-in behavior for an upgrade run.
type UpgradeOptions struct {
	// Preflight enables the pre-execution checks that abort the whole batch when any
	// candidate extension cannot be upgraded.
	Preflight bool
}

// UpgradeRecord is the per-extension result of an upgrade run. The same fields are used
// for human-readable output, machine-readable output, and the on-disk transaction journal.
type UpgradeRecord struct {
	// Name is the extension name without the gh- prefix.
	Name string
	// CurrentVersion is the version before the upgrade.
	CurrentVersion string
	// TargetVersion is the version the upgrade intended to move to.
	TargetVersion string
	// Outcome is the result category.
	Outcome UpgradeOutcome
	// Reason is a machine-readable reason such as a SkipReason* or PreflightReason* value.
	Reason string
	// Message is a human-readable detail message.
	Message string
}

// UpgradeReport is the result of one upgrade run.
type UpgradeReport struct {
	// TransactionID is the unique identifier of the transaction for this run.
	TransactionID string
	// Records holds one entry per processed extension, including startup reconciliation
	// records from previous interrupted runs.
	Records []UpgradeRecord
}

//go:generate moq -rm -out manager_mock.go . ExtensionManager
type ExtensionManager interface {
	List() []Extension
	Install(ghrepo.Interface, string) error
	InstallLocal(dir string) error
	Upgrade(name string, force bool) error
	// UpgradeWithReport behaves like Upgrade but also returns the structured per-extension
	// report built from the run's transaction journal.
	UpgradeWithReport(name string, force bool, opts UpgradeOptions) (*UpgradeReport, error)
	Remove(name string) error
	Dispatch(args []string, stdin io.Reader, stdout, stderr io.Writer) (bool, error)
	Create(name string, tmplType ExtTemplateType) error
	EnableDryRunMode()
	UpdateDir(name string) string
}
