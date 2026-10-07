package extension

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/cli/cli/v2/internal/flock"
	"github.com/cli/cli/v2/pkg/extensions"
	"github.com/google/uuid"
	"gopkg.in/yaml.v3"
)

// txStatus is the state of an upgrade transaction.
type txStatus string

const (
	// txStatusPrepared indicates the journal has been written but execution has not
	// necessarily finished; this is the state a crashed run leaves behind.
	txStatusPrepared txStatus = "prepared"
	// txStatusRunning indicates the execution phase is in progress.
	txStatusRunning txStatus = "running"
	// txStatusCompleted indicates every record reached a terminal outcome.
	txStatusCompleted txStatus = "completed"
)

const (
	journalKindBinary = "binary"
	journalKindGit    = "git"
)

// journalRecord is the per-extension record persisted in an upgrade transaction journal.
// It carries everything an out-of-process reader needs to verify or restore the upgrade.
type journalRecord struct {
	// Name is the extension name without the gh- prefix.
	Name string `yaml:"name"`
	// Kind is the pre-upgrade kind: journalKindBinary or journalKindGit.
	Kind string `yaml:"kind"`
	// FromVersion is the version before the upgrade.
	FromVersion string `yaml:"from_version"`
	// ToVersion is the version the upgrade intends to move to.
	ToVersion string `yaml:"to_version"`
	// TargetDir is the absolute path of the installed extension directory.
	TargetDir string `yaml:"target_dir"`
	// BackupDir is the absolute path where replaced content was moved to.
	BackupDir string `yaml:"backup_dir,omitempty"`
	// StagingDir is the absolute path where new content is prepared before the swap.
	StagingDir string `yaml:"staging_dir,omitempty"`
	// BackupRef is the local git ref holding the pre-upgrade commit for script extensions.
	BackupRef string `yaml:"backup_ref,omitempty"`
	// Outcome is the terminal result category; empty while the record is unfinished.
	Outcome string `yaml:"outcome,omitempty"`
	// Reason is a machine-readable reason, such as a skip or preflight reason.
	Reason string `yaml:"reason,omitempty"`
	// Message is a human-readable detail message.
	Message string `yaml:"message,omitempty"`
}

// journal is the on-disk record of one upgrade run. It is written before any extension
// content changes and is verifiable outside the process.
type journal struct {
	// ID is the unique identifier of this transaction.
	ID string `yaml:"id"`
	// StartedAt is when the transaction was created.
	StartedAt time.Time `yaml:"started_at"`
	// Scope is "all" for a batch upgrade or the extension name for a single upgrade.
	Scope string `yaml:"scope"`
	// Force records whether the run was started with --force.
	Force bool `yaml:"force"`
	// Preflight records whether the run was started with --preflight.
	Preflight bool `yaml:"preflight"`
	// Status is the transaction state.
	Status txStatus `yaml:"status"`
	// Records holds one entry per processed extension.
	Records []journalRecord `yaml:"records"`
}

// transactionsDir returns the directory holding upgrade transaction journals.
func (m *Manager) transactionsDir() string {
	return filepath.Join(m.updateDir(), "upgrade-transactions")
}

// transactionDir returns the journal directory for one transaction.
func (m *Manager) transactionDir(txID string) string {
	return filepath.Join(m.transactionsDir(), txID)
}

// journalPath returns the journal file path for one transaction.
func (m *Manager) journalPath(txID string) string {
	return filepath.Join(m.transactionDir(txID), "journal.yml")
}

// upgradeLockPath returns the path of the machine-local upgrade mutual-exclusion lock.
func (m *Manager) upgradeLockPath() string {
	return filepath.Join(m.updateDir(), "upgrade.lock")
}

// workDir returns the same-volume hidden working directory for one transaction. It sits
// under the install directory so renames to and from extension directories are atomic,
// and its name does not start with gh- so list() never reports it as an extension.
func (m *Manager) workDir(txID string) string {
	return filepath.Join(m.installDir(), ".upgrade-work", txID)
}

// backupDir returns the per-extension directory holding the replaced pre-upgrade content.
func (m *Manager) backupDir(txID, name string) string {
	return filepath.Join(m.workDir(txID), "backup", normalizeExtension(name))
}

// stagingDir returns the per-extension directory holding new content before it is swapped
// into place. Its base name is the normalized extension name so it can be renamed directly
// to the target directory.
func (m *Manager) stagingDir(txID, name string) string {
	return filepath.Join(m.workDir(txID), "staging", normalizeExtension(name))
}

// acquireUpgradeLock takes the machine-local, non-blocking exclusive lock that
// serializes extension upgrades. Single and batch upgrades use the same lock, and the
// OS releases it automatically if the process is terminated.
func (m *Manager) acquireUpgradeLock() (func(), error) {
	if err := os.MkdirAll(m.updateDir(), 0755); err != nil {
		return nil, fmt.Errorf("failed to create extension state directory: %w", err)
	}
	_, unlock, err := flock.TryLock(m.upgradeLockPath())
	if err != nil {
		if errors.Is(err, flock.ErrLocked) {
			return nil, fmt.Errorf("another extension upgrade is already running; wait for it to finish and try again: %w", err)
		}
		return nil, fmt.Errorf("failed to acquire upgrade lock: %w", err)
	}
	return unlock, nil
}

// newJournal creates a transaction with a fresh ID and writes its journal to disk before
// any extension content is changed.
func (m *Manager) newJournal(scope string, force, preflight bool, records []journalRecord) (*journal, error) {
	j := &journal{
		ID:        uuid.NewString(),
		StartedAt: time.Now(),
		Scope:     scope,
		Force:     force,
		Preflight: preflight,
		Status:    txStatusPrepared,
		Records:   records,
	}
	if err := m.writeJournal(j); err != nil {
		return nil, err
	}
	return j, nil
}

// writeJournal serializes the journal and replaces the on-disk file atomically.
func (m *Manager) writeJournal(j *journal) error {
	if err := os.MkdirAll(m.transactionDir(j.ID), 0755); err != nil {
		return fmt.Errorf("failed to create transaction directory: %w", err)
	}
	data, err := yaml.Marshal(j)
	if err != nil {
		return fmt.Errorf("failed to serialize journal: %w", err)
	}
	if err := writeFileAtomic(m.journalPath(j.ID), data, 0600); err != nil {
		return err
	}
	return nil
}

// loadJournal reads and parses the journal for one transaction.
func (m *Manager) loadJournal(txID string) (*journal, error) {
	data, err := os.ReadFile(m.journalPath(txID))
	if err != nil {
		return nil, err
	}
	var j journal
	if err := yaml.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("failed to parse journal %s: %w", txID, err)
	}
	return &j, nil
}

// loadJournals scans the transactions directory and loads every journal.
func (m *Manager) loadJournals() ([]*journal, error) {
	entries, err := os.ReadDir(m.transactionsDir())
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var journals []*journal
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		j, err := m.loadJournal(entry.Name())
		if err != nil {
			return nil, err
		}
		journals = append(journals, j)
	}
	return journals, nil
}

// toUpgradeRecord maps a persisted record to the structured result used by command and
// machine-readable output.
func (r journalRecord) toUpgradeRecord() extensions.UpgradeRecord {
	return extensions.UpgradeRecord{
		Name:           r.Name,
		CurrentVersion: r.FromVersion,
		TargetVersion:  r.ToVersion,
		Outcome:        extensions.UpgradeOutcome(r.Outcome),
		Reason:         r.Reason,
		Message:        r.Message,
	}
}

// writeFileAtomic writes data to a temporary file in the target directory and renames it
// over the destination, so readers never observe a partially written file.
func writeFileAtomic(dest string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(dest)
	tmp, err := os.CreateTemp(dir, ".journal-tmp-*")
	if err != nil {
		return fmt.Errorf("failed to create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return fmt.Errorf("failed to write temporary file: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		cleanup()
		return fmt.Errorf("failed to set temporary file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to close temporary file: %w", err)
	}
	if err := os.Rename(tmpName, dest); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("failed to replace %s: %w", dest, err)
	}
	return nil
}
