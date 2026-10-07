package doctor

import (
	"bytes"
	"testing"

	"github.com/cli/cli/v2/internal/config"
	"github.com/cli/cli/v2/internal/gh"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewCmdConfigDoctor(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantsErr bool
	}{
		{
			name:  "no arguments",
			input: "",
		},
		{
			name:     "unexpected argument",
			input:    "something",
			wantsErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ios, _, _, _ := iostreams.Test()
			cfg := config.NewMockConfig()
			f := &cmdutil.Factory{
				IOStreams: ios,
				Config: func() (gh.Config, error) {
					return cfg, nil
				},
			}

			argv := []string{}
			if tt.input != "" {
				argv = []string{tt.input}
			}

			cmd := NewCmdConfigDoctor(f, func(*DoctorOptions) error { return nil })
			cmd.Flags().BoolP("help", "x", false, "")
			cmd.SetArgs(argv)
			cmd.SetIn(&bytes.Buffer{})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})

			_, err := cmd.ExecuteC()
			if tt.wantsErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestDoctorRun(t *testing.T) {
	t.Run("clean configuration passes", func(t *testing.T) {
		ios, _, stdout, stderr := iostreams.Test()
		cfg, _ := config.NewIsolatedTestConfig(t, "")

		err := doctorRun(&DoctorOptions{
			IO:     ios,
			Config: cfg,
		})
		require.NoError(t, err)
		assert.Equal(t, "\u2713 No configuration problems found.\n", stdout.String())
		assert.Empty(t, stderr.String())
	})

	t.Run("problematic configuration reports and fails", func(t *testing.T) {
		ios, _, stdout, stderr := iostreams.Test()

		cfg, _ := config.NewIsolatedTestConfig(t, `
aliases:
  repo x: repo x
git_protocol: svn
zzz: true
`)

		err := doctorRun(&DoctorOptions{
			IO:     ios,
			Config: cfg,
		})
		require.Equal(t, cmdutil.SilentError, err)

		output := stdout.String()
		assert.Contains(t, output, "X global: key \"git_protocol\" has invalid value \"svn\"")
		assert.Contains(t, output, "X global: unknown key \"zzz\"")
		assert.Contains(t, output, "X alias \"repo x\" is self-referential")
		assert.Empty(t, stderr.String())
	})
}
