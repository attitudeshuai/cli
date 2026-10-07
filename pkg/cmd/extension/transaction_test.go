package extension

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/cli/cli/v2/internal/flock"
	"github.com/cli/cli/v2/pkg/extensions"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManager_NewJournalRoundTrip(t *testing.T) {
	dataDir := t.TempDir()
	updateDir := t.TempDir()
	m := newTestManager(dataDir, updateDir, nil, nil, nil)

	records := []journalRecord{
		{
			Name:        "hello",
			Kind:        journalKindBinary,
			FromVersion: "v1.0.0",
			ToVersion:   "v1.1.0",
			TargetDir:   filepath.Join(dataDir, "extensions", "gh-hello"),
			BackupDir:   filepath.Join(dataDir, "extensions", ".upgrade-work", "placeholder", "backup", "gh-hello"),
			StagingDir:  filepath.Join(dataDir, "extensions", ".upgrade-work", "placeholder", "staging", "gh-hello"),
		},
	}
	j, err := m.newJournal("all", false, true, records)
	require.NoError(t, err)

	assert.NotEmpty(t, j.ID)
	assert.Equal(t, txStatusPrepared, j.Status)
	assert.FileExists(t, m.journalPath(j.ID))

	loaded, err := m.loadJournal(j.ID)
	require.NoError(t, err)
	assert.Equal(t, j.ID, loaded.ID)
	assert.Equal(t, "all", loaded.Scope)
	assert.True(t, loaded.Preflight)
	assert.False(t, loaded.Force)
	require.Len(t, loaded.Records, 1)
	r := loaded.Records[0]
	assert.Equal(t, "hello", r.Name)
	assert.Equal(t, journalKindBinary, r.Kind)
	assert.Equal(t, "v1.0.0", r.FromVersion)
	assert.Equal(t, "v1.1.0", r.ToVersion)
	assert.Equal(t, records[0].BackupDir, r.BackupDir)
	assert.Equal(t, records[0].StagingDir, r.StagingDir)
}

func TestManager_WriteJournalUpdates(t *testing.T) {
	dataDir := t.TempDir()
	updateDir := t.TempDir()
	m := newTestManager(dataDir, updateDir, nil, nil, nil)

	j, err := m.newJournal("hello", true, false, []journalRecord{{Name: "hello"}})
	require.NoError(t, err)

	j.Status = txStatusCompleted
	j.Records[0].Outcome = "upgraded"
	require.NoError(t, m.writeJournal(j))

	loaded, err := m.loadJournal(j.ID)
	require.NoError(t, err)
	assert.Equal(t, txStatusCompleted, loaded.Status)
	assert.Equal(t, "upgraded", loaded.Records[0].Outcome)
}

func TestManager_LoadJournals(t *testing.T) {
	dataDir := t.TempDir()
	updateDir := t.TempDir()
	m := newTestManager(dataDir, updateDir, nil, nil, nil)

	j1, err := m.newJournal("all", false, false, nil)
	require.NoError(t, err)
	j2, err := m.newJournal("all", false, false, nil)
	require.NoError(t, err)

	journals, err := m.loadJournals()
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{j1.ID, j2.ID}, []string{journals[0].ID, journals[1].ID})
}

func TestManager_WorkDirHiddenFromList(t *testing.T) {
	dataDir := t.TempDir()
	updateDir := t.TempDir()
	extDir := filepath.Join(dataDir, "extensions", "gh-hello")
	require.NoError(t, stubExtension(filepath.Join(extDir, "gh-hello")))
	gc, gcOne := &mockGitClient{}, &mockGitClient{}
	gc.On("ForRepo", extDir).Return(gcOne).Once()
	m := newTestManager(dataDir, updateDir, nil, gc, nil)

	j, err := m.newJournal("all", false, false, nil)
	require.NoError(t, err)

	// Populate same-volume backup and staging content for one extension.
	require.NoError(t, os.MkdirAll(m.backupDir(j.ID, "hello"), 0755))
	require.NoError(t, os.MkdirAll(m.stagingDir(j.ID, "hello"), 0755))

	exts, err := m.list(false)
	require.NoError(t, err)
	assert.Len(t, exts, 1)
	assert.Equal(t, "hello", exts[0].Name())
	gc.AssertExpectations(t)
}

func TestManager_AcquireUpgradeLock(t *testing.T) {
	dataDir := t.TempDir()
	updateDir := t.TempDir()
	m := newTestManager(dataDir, updateDir, nil, nil, nil)

	unlock, err := m.acquireUpgradeLock()
	require.NoError(t, err)

	_, err = m.acquireUpgradeLock()
	require.Error(t, err)
	assert.True(t, errors.Is(err, flock.ErrLocked))
	assert.Contains(t, err.Error(), "another extension upgrade is already running")

	unlock()

	unlock2, err := m.acquireUpgradeLock()
	require.NoError(t, err)
	unlock2()
}

func TestManager_UpgradeWithReport_LockContention(t *testing.T) {
	dataDir := t.TempDir()
	updateDir := t.TempDir()
	require.NoError(t, stubExtension(filepath.Join(dataDir, "extensions", "gh-hello", "gh-hello")))
	m := newTestManager(dataDir, updateDir, nil, nil, nil)

	unlock, err := m.acquireUpgradeLock()
	require.NoError(t, err)
	defer unlock()

	report, err := m.UpgradeWithReport("", false, extensions.UpgradeOptions{})
	require.Error(t, err)
	assert.Nil(t, report)
	assert.NoDirExists(t, m.transactionsDir())
}

func TestJournalRecord_ToUpgradeRecord(t *testing.T) {
	r := journalRecord{
		Name:        "hello",
		FromVersion: "v1.0.0",
		ToVersion:   "v1.1.0",
		Outcome:     "skipped",
		Reason:      "pinned",
		Message:     "pinned extensions can not be upgraded",
	}
	got := r.toUpgradeRecord()
	assert.Equal(t, "hello", got.Name)
	assert.Equal(t, "v1.0.0", got.CurrentVersion)
	assert.Equal(t, "v1.1.0", got.TargetVersion)
	assert.Equal(t, "skipped", string(got.Outcome))
	assert.Equal(t, "pinned", got.Reason)
	assert.Equal(t, "pinned extensions can not be upgraded", got.Message)
}
