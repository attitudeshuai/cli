package run

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/api"
	fd "github.com/cli/cli/v2/internal/featuredetection"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/internal/prompter"
	"github.com/cli/cli/v2/pkg/cmd/workflow/shared"
	"github.com/cli/cli/v2/pkg/cmdutil"
	"github.com/cli/cli/v2/pkg/httpmock"
	"github.com/cli/cli/v2/pkg/iostreams"
	"github.com/google/shlex"
	"github.com/stretchr/testify/assert"
)

func TestNewCmdRun(t *testing.T) {
	tests := []struct {
		name     string
		cli      string
		tty      bool
		wants    RunOptions
		wantsErr bool
		errMsg   string
		stdin    string
	}{
		{
			name:     "blank nontty",
			wantsErr: true,
			errMsg:   "workflow ID, name, or filename required when not running interactively",
		},
		{
			name: "blank tty",
			tty:  true,
			wants: RunOptions{
				Prompt: true,
			},
		},
		{
			name: "ref flag",
			tty:  true,
			cli:  "--ref 12345abc",
			wants: RunOptions{
				Prompt: true,
				Ref:    "12345abc",
			},
		},
		{
			name: "both STDIN and input fields",
			stdin: "some json",
			cli:  "workflow.yml -fhey=there --json",
			wants: RunOptions{
				Selector:  "workflow.yml",
				RawFields: []string{"hey=there"},
				JSON:      true,
				JSONInput: "some json",
			},
		},
		{
			name: "-f args",
			tty:  true,
			cli:  `workflow.yml -fhey=there -fname="dana scully"`,
			wants: RunOptions{
				Selector:  "workflow.yml",
				RawFields: []string{"hey=there", "name=dana scully"},
			},
		},
		{
			name: "-F args",
			tty:  true,
			cli:  `workflow.yml -Fhey=there -Fname="dana scully" -Ffile=@cool.txt`,
			wants: RunOptions{
				Selector:    "workflow.yml",
				MagicFields: []string{"hey=there", "name=dana scully", "file=@cool.txt"},
			},
		},
		{
			name: "-F/-f arg mix",
			tty:  true,
			cli:  `workflow.yml -fhey=there -Fname="dana scully" -Ffile=@cool.txt`,
			wants: RunOptions{
				Selector:    "workflow.yml",
				RawFields:   []string{"hey=there"},
				MagicFields: []string{`name=dana scully`, "file=@cool.txt"},
			},
		},
		{
			name:  "json on STDIN",
			cli:   "workflow.yml --json",
			stdin: `{"cool":"yeah"}`,
			wants: RunOptions{
				JSON:      true,
				JSONInput: `{"cool":"yeah"}`,
				Selector:  "workflow.yml",
			},
		},
		{
			name: "dry run",
			tty:  true,
			cli:  "workflow.yml --dry-run",
			wants: RunOptions{
				Selector:      "workflow.yml",
				DryRun:        true,
				DryRunFormat:  "human",
				OnDefinitionError: "proceed",
			},
		},
		{
			name:  "dry run json format with fields",
			cli:   "workflow.yml -fhey=there --dry-run --dry-run-format json",
			stdin: "",
			tty:   true,
			wants: RunOptions{
				Selector:      "workflow.yml",
				RawFields:     []string{"hey=there"},
				DryRun:        true,
				DryRunFormat:  "json",
				OnDefinitionError: "proceed",
			},
		},
		{
			name:     "dry run format requires dry run",
			cli:      "workflow.yml --dry-run-format json",
			tty:      true,
			wantsErr: true,
			errMsg:   "--dry-run-format can only be used with --dry-run",
		},
		{
			name: "on definition error stop",
			tty:  true,
			cli:  "workflow.yml --on-definition-error stop",
			wants: RunOptions{
				Selector:          "workflow.yml",
				OnDefinitionError: "stop",
			},
		},
		{
			name:     "on definition error invalid value",
			cli:      "workflow.yml --on-definition-error whatever",
			tty:      true,
			wantsErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ios, stdin, _, _ := iostreams.Test()
			if tt.stdin == "" {
				ios.SetStdinTTY(tt.tty)
			} else {
				stdin.WriteString(tt.stdin)
			}
			ios.SetStdoutTTY(tt.tty)

			f := &cmdutil.Factory{
				IOStreams: ios,
			}

			argv, err := shlex.Split(tt.cli)
			assert.NoError(t, err)

			var gotOpts *RunOptions
			cmd := NewCmdRun(f, func(opts *RunOptions) error {
				gotOpts = opts
				return nil
			})
			cmd.SetArgs(argv)
			cmd.SetIn(&bytes.Buffer{})
			cmd.SetOut(io.Discard)
			cmd.SetErr(io.Discard)

			_, err = cmd.ExecuteC()
			if tt.wantsErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Equal(t, tt.errMsg, err.Error())
				}
				return
			}

			assert.NoError(t, err)

			assert.Equal(t, tt.wants.Selector, gotOpts.Selector)
			assert.Equal(t, tt.wants.Prompt, gotOpts.Prompt)
			assert.Equal(t, tt.wants.JSONInput, gotOpts.JSONInput)
			assert.Equal(t, tt.wants.JSON, gotOpts.JSON)
			assert.Equal(t, tt.wants.Ref, gotOpts.Ref)
			assert.Equal(t, tt.wants.DryRun, gotOpts.DryRun)
			wantDryRunFormat := tt.wants.DryRunFormat
			if wantDryRunFormat == "" {
				wantDryRunFormat = dryRunFormatHuman
			}
			assert.Equal(t, wantDryRunFormat, gotOpts.DryRunFormat)
			wantPolicy := tt.wants.OnDefinitionError
			if wantPolicy == "" {
				wantPolicy = definitionErrorProceed
			}
			assert.Equal(t, wantPolicy, gotOpts.OnDefinitionError)
			assert.ElementsMatch(t, tt.wants.RawFields, gotOpts.RawFields)
			assert.ElementsMatch(t, tt.wants.MagicFields, gotOpts.MagicFields)
		})
	}
}

func Test_magicFieldValue(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "gh-test")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	fmt.Fprint(f, "file contents")

	ios, _, _, _ := iostreams.Test()

	type args struct {
		v    string
		opts RunOptions
	}
	tests := []struct {
		name    string
		args    args
		want    any
		wantErr bool
	}{
		{
			name:    "string",
			args:    args{v: "hello"},
			want:    "hello",
			wantErr: false,
		},
		{
			name: "file",
			args: args{
				v:    "@" + f.Name(),
				opts: RunOptions{IO: ios},
			},
			want:    "file contents",
			wantErr: false,
		},
		{
			name: "file error",
			args: args{
				v:    "@",
				opts: RunOptions{IO: ios},
			},
			want:    nil,
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := magicFieldValue(tt.args.v, tt.args.opts)
			if (err != nil) != tt.wantErr {
				t.Errorf("magicFieldValue() error = %v, wantErr %v", err, tt.wantErr)
				return
			}
			if tt.wantErr {
				return
			}
			assert.Equal(t, tt.want, got)
		})
	}
}

func Test_findInputs(t *testing.T) {
	tests := []struct {
		name    string
		YAML    []byte
		wantErr bool
		errMsg  string
		wantOut []WorkflowInput
	}{
		{
			name:    "blank",
			YAML:    []byte{},
			wantErr: true,
			errMsg:  "invalid YAML file",
		},
		{
			name:    "no event specified",
			YAML:    []byte("name: workflow"),
			wantErr: true,
			errMsg:  "invalid workflow: no 'on' key",
		},
		{
			name:    "not workflow_dispatch",
			YAML:    []byte("name: workflow\non: pull_request"),
			wantErr: true,
			errMsg:  "unable to manually run a workflow without a workflow_dispatch event",
		},
		{
			name:    "bad inputs",
			YAML:    []byte("name: workflow\non:\n workflow_dispatch:\n  inputs: lol  "),
			wantErr: true,
			errMsg:  "could not decode workflow inputs: yaml: unmarshal errors:\n  line 4: cannot unmarshal !!str `lol` into map[string]run.WorkflowInput",
		},
		{
			name:    "short syntax",
			YAML:    []byte("name: workflow\non: workflow_dispatch"),
			wantOut: []WorkflowInput{},
		},
		{
			name:    "array of events",
			YAML:    []byte("name: workflow\non: [pull_request, workflow_dispatch]\n"),
			wantOut: []WorkflowInput{},
		},
		{
			name: "inputs",
			YAML: []byte(`name: workflow
on:
  workflow_dispatch:
    inputs:
      foo:
        required: true
        description: good foo
      bar:
        default: boo
      baz:
        description: it's baz
      quux:
        required: true
        default: "cool"
jobs:
  yell:
    runs-on: ubuntu-latest
    steps:
      - name: echo
        run: |
          echo "echo"`),
			wantOut: []WorkflowInput{
				{
					Name:    "bar",
					Default: "boo",
				},
				{
					Name:        "baz",
					Description: "it's baz",
				},
				{
					Name:        "foo",
					Required:    true,
					Description: "good foo",
				},
				{
					Name:     "quux",
					Required: true,
					Default:  "cool",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := findInputs(tt.YAML)
			if tt.wantErr {
				assert.Error(t, err)
				if err != nil {
					assert.Equal(t, tt.errMsg, err.Error())
				}
				return
			} else {
				assert.NoError(t, err)
			}

			assert.Equal(t, tt.wantOut, result)
		})
	}

}

func TestRun(t *testing.T) {
	noInputsYAMLContent := []byte(`
name: minimal workflow
on: workflow_dispatch
jobs:
  yell:
    runs-on: ubuntu-latest
    steps:
      - name: do a yell
        run: |
          echo "AUUUGH!"
`)
	encodedNoInputsYAMLContent := base64.StdEncoding.EncodeToString(noInputsYAMLContent)
	yamlContent := []byte(`
name: a workflow
on:
  workflow_dispatch:
    inputs:
      greeting:
        default: hi
        description: a greeting
      name:
        required: true
        description: a name
jobs:
  greet:
    runs-on: ubuntu-latest
    steps:
      - name: perform the greet
        run: |
          echo "${{ github.event.inputs.greeting}}, ${{ github.events.inputs.name }}!"`)

	encodedYAMLContent := base64.StdEncoding.EncodeToString(yamlContent)

	yamlContentChoiceIp := []byte(`
name: choice inputs
on:
  workflow_dispatch:
    inputs:
      name:
        type: choice
        description: Who to greet
        default: monalisa
        options:
        - monalisa
        - cschleiden
      favourite-animal:
        type: choice
        description: What's your favourite animal
        required: true
        options:
        - dog
        - cat
jobs:
  greet:
  runs-on: ubuntu-latest
  steps:
  - name: Send greeting
    run: echo "${{ github.event.inputs.message }} ${{ fromJSON('["", "🥳"]')[github.event.inputs.use-emoji == 'true'] }} ${{ github.event.inputs.name }}"`)
	encodedYAMLContentChoiceIp := base64.StdEncoding.EncodeToString(yamlContentChoiceIp)

	yamlContentMissingChoiceIp := []byte(`
name: choice missing inputs
on:
  workflow_dispatch:
    inputs:
      name:
        type: choice
        description: Who to greet
        options:
jobs:
  greet:
  runs-on: ubuntu-latest
  steps:
  - name: Send greeting
    run: echo "${{ github.event.inputs.message }} ${{ fromJSON('["", "🥳"]')[github.event.inputs.use-emoji == 'true'] }} ${{ github.event.inputs.name }}"`)
	encodedYAMLContentMissingChoiceIp := base64.StdEncoding.EncodeToString(yamlContentMissingChoiceIp)

	// A workflow that declares optional "greeting" and "name" inputs, so any
	// subset of the happy-path inputs reconciles cleanly.
	reconcilableYAMLContent := []byte(`
name: a workflow
on:
  workflow_dispatch:
    inputs:
      greeting:
        type: string
      name:
        type: string
jobs:
  greet:
    runs-on: ubuntu-latest
`)
	encodedReconcilableYAMLContent := base64.StdEncoding.EncodeToString(reconcilableYAMLContent)

	// A workflow that declares a required choice input "env".
	choiceYAMLContent := []byte(`
name: choice workflow
on:
  workflow_dispatch:
    inputs:
      env:
        type: choice
        required: true
        options:
        - dev
        - prod
jobs:
  greet:
    runs-on: ubuntu-latest
`)
	encodedChoiceYAMLContent := base64.StdEncoding.EncodeToString(choiceYAMLContent)

	// A workflow file without a workflow_dispatch event.
	notDispatchYAMLContent := []byte("name: workflow\non: pull_request\n")
	encodedNotDispatchYAMLContent := base64.StdEncoding.EncodeToString(notDispatchYAMLContent)

	// A workflow file whose inputs section cannot be parsed.
	malformedYAMLContent := []byte("name: workflow\non:\n workflow_dispatch:\n  inputs: lol\n")
	encodedMalformedYAMLContent := base64.StdEncoding.EncodeToString(malformedYAMLContent)

	registerWorkflow := func(reg *httpmock.Registry) {
		reg.Register(
			httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows/workflow.yml"),
			httpmock.JSONResponse(shared.Workflow{
				Path: ".github/workflows/workflow.yml",
				ID:   12345,
			}))
	}
	registerContents := func(reg *httpmock.Registry, encoded string) {
		reg.Register(
			httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
			httpmock.JSONResponse(struct{ Content string }{
				Content: encoded,
			}))
	}

	// Old GitHub API servers return 204 No Content for successful workflow dispatches.
	stubs := func(reg *httpmock.Registry) {
		registerWorkflow(reg)
		registerContents(reg, encodedReconcilableYAMLContent)
		reg.Register(
			httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
			httpmock.StatusStringResponse(204, "cool"))
	}

	// Current GitHub API servers return 200 OK with run info for successful workflow dispatches,
	// if `return_run_details` is enabled in the request body.
	stubsWithRunInfo := func(reg *httpmock.Registry) {
		registerWorkflow(reg)
		registerContents(reg, encodedReconcilableYAMLContent)
		reg.Register(
			httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
			httpmock.StatusJSONResponse(200, map[string]any{
				"workflow_run_id": int64(6789),
				"run_url":         "https://api.github.com/repos/OWNER/REPO/actions/runs/6789",
				"html_url":        "https://github.com/OWNER/REPO/actions/runs/6789",
			}))
	}

	tests := []struct {
		name           string
		opts           *RunOptions
		tty            bool
		wantErr        bool
		errOut         string
		wantOut        string
		wantErrOut     string
		wantBody       map[string]any
		wantNoDispatch bool
		httpStubs      func(*httpmock.Registry)
		promptStubs    func(*prompter.MockPrompter)
	}{
		{
			name: "bad JSON",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"bad":"corrupt"`,
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows/workflow.yml"),
					httpmock.JSONResponse(shared.Workflow{
						Path: ".github/workflows/workflow.yml",
					}))
			},
			wantErr: true,
			errOut:  "could not parse provided JSON: unexpected end of JSON input",
		},
		{
			// TODO workflowDispatchRunDetailsCleanup
			// To be deleted
			name: "good JSON without run info (204)",
			tty:  true,
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Detector:  &fd.DisabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name": "scully",
				},
				"ref": "trunk",
			},
			httpStubs: stubs,
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at trunk

				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			name: "good JSON with run info",
			tty:  true,
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Detector:  &fd.EnabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name": "scully",
				},
				"ref":                "trunk",
				"return_run_details": true,
			},
			httpStubs: stubsWithRunInfo,
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at trunk
				https://github.com/OWNER/REPO/actions/runs/6789

				To see the created workflow run, try: gh run view 6789
				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			// TODO workflowDispatchRunDetailsCleanup
			// To be deleted
			name: "nontty good JSON without run info (204)",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Detector:  &fd.DisabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name": "scully",
				},
				"ref": "trunk",
			},
			httpStubs: stubs,
		},
		{
			name: "nontty good JSON with run info",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Detector:  &fd.EnabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name": "scully",
				},
				"ref":                "trunk",
				"return_run_details": true,
			},
			httpStubs: stubsWithRunInfo,
			wantOut:   "https://github.com/OWNER/REPO/actions/runs/6789\n",
		},
		{
			// TODO workflowDispatchRunDetailsCleanup
			// To be deleted
			name: "nontty good input fields without run info (204)",
			opts: &RunOptions{
				Selector:    "workflow.yml",
				RawFields:   []string{`name=scully`},
				MagicFields: []string{`greeting=hey`},
				Detector:    &fd.DisabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name":     "scully",
					"greeting": "hey",
				},
				"ref": "trunk",
			},
			httpStubs: stubs,
		},
		{
			name: "nontty good input fields with run info",
			opts: &RunOptions{
				Selector:    "workflow.yml",
				RawFields:   []string{`name=scully`},
				MagicFields: []string{`greeting=hey`},
				Detector:    &fd.EnabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name":     "scully",
					"greeting": "hey",
				},
				"ref":                "trunk",
				"return_run_details": true,
			},
			httpStubs: stubsWithRunInfo,
			wantOut:   "https://github.com/OWNER/REPO/actions/runs/6789\n",
		},
		{
			// TODO workflowDispatchRunDetailsCleanup
			// To be deleted
			name: "respects ref, without run info (204)",
			tty:  true,
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Ref:       "good-branch",
				Detector:  &fd.DisabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name": "scully",
				},
				"ref": "good-branch",
			},
			httpStubs: stubs,
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at good-branch

				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			name: "respects ref, with run info",
			tty:  true,
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Ref:       "good-branch",
				Detector:  &fd.EnabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name": "scully",
				},
				"ref":                "good-branch",
				"return_run_details": true,
			},
			httpStubs: stubsWithRunInfo,
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at good-branch
				https://github.com/OWNER/REPO/actions/runs/6789

				To see the created workflow run, try: gh run view 6789
				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			name: "missing required input is blocked before dispatch",
			tty:  true,
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"greeting":"hello there"}`,
				Detector:  &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				// yamlContent declares "name" as required with no default.
				registerContents(reg, encodedYAMLContent)
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut:         "cannot dispatch workflow.yml at trunk: provided inputs do not match its workflow_dispatch declaration:\n- required input \"name\" was not provided and declares no default",
		},
		{
			name: "yaml file extension",
			tty:  false,
			opts: &RunOptions{
				Selector: "workflow.yaml",
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows/workflow.yaml"),
					httpmock.StatusStringResponse(200, `{"id": 12345}`))
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusJSONResponse(200, map[string]any{
						"workflow_run_id": int64(6789),
						"run_url":         "https://api.github.com/repos/OWNER/REPO/actions/runs/6789",
						"html_url":        "https://github.com/OWNER/REPO/actions/runs/6789",
					}))
			},
			wantBody: map[string]any{
				"inputs":             map[string]any{},
				"ref":                "trunk",
				"return_run_details": true,
			},
			wantErr: false,
			wantOut: "https://github.com/OWNER/REPO/actions/runs/6789\n",
		},
		{
			name: "input fields missing required are blocked before dispatch",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				RawFields: []string{`greeting="hello there"`},
				Detector:  &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedYAMLContent)
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut:         "cannot dispatch workflow.yml at trunk: provided inputs do not match its workflow_dispatch declaration:\n- required input \"name\" was not provided and declares no default",
		},
		{
			name: "prompt, no workflows enabled",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{
							{
								Name:  "disabled",
								State: shared.DisabledManually,
								ID:    102,
							},
						},
					}))
			},
			wantErr: true,
			errOut:  "no workflows are enabled on this repository",
		},
		{
			name: "prompt, no workflows",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{},
					}))
			},
			wantErr: true,
			errOut:  "could not fetch workflows for OWNER/REPO: no workflows are enabled",
		},
		{
			// TODO workflowDispatchRunDetailsCleanup
			// To be deleted
			name: "prompt, minimal yaml, without run info (204)",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.DisabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{
							{
								Name:  "minimal workflow",
								ID:    1,
								State: shared.Active,
								Path:  ".github/workflows/minimal.yml",
							},
						},
					}))
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fminimal.yml"),
					httpmock.JSONResponse(struct{ Content string }{
						Content: encodedNoInputsYAMLContent,
					}))
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/1/dispatches"),
					httpmock.StatusStringResponse(204, "cool"))
			},
			promptStubs: func(pm *prompter.MockPrompter) {
				pm.RegisterSelect("Select a workflow", []string{"minimal workflow (minimal.yml)"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
			},
			wantBody: map[string]any{
				"inputs": map[string]any{},
				"ref":    "trunk",
			},
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for minimal.yml at trunk

				To see runs for this workflow, try: gh run list --workflow="minimal.yml"
			`),
		},
		{
			name: "prompt, minimal yaml, with run info",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{
							{
								Name:  "minimal workflow",
								ID:    1,
								State: shared.Active,
								Path:  ".github/workflows/minimal.yml",
							},
						},
					}))
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fminimal.yml"),
					httpmock.JSONResponse(struct{ Content string }{
						Content: encodedNoInputsYAMLContent,
					}))
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/1/dispatches"),
					httpmock.StatusJSONResponse(200, map[string]any{
						"workflow_run_id": int64(6789),
						"run_url":         "https://api.github.com/repos/OWNER/REPO/actions/runs/6789",
						"html_url":        "https://github.com/OWNER/REPO/actions/runs/6789",
					}))
			},
			promptStubs: func(pm *prompter.MockPrompter) {
				pm.RegisterSelect("Select a workflow", []string{"minimal workflow (minimal.yml)"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
			},
			wantBody: map[string]any{
				"inputs":             map[string]any{},
				"ref":                "trunk",
				"return_run_details": true,
			},
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for minimal.yml at trunk
				https://github.com/OWNER/REPO/actions/runs/6789

				To see the created workflow run, try: gh run view 6789
				To see runs for this workflow, try: gh run list --workflow="minimal.yml"
			`),
		},
		{
			// TODO workflowDispatchRunDetailsCleanup
			// To be deleted
			name: "prompt without run info (204)",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.DisabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{
							{
								Name:  "a workflow",
								ID:    12345,
								State: shared.Active,
								Path:  ".github/workflows/workflow.yml",
							},
						},
					}))
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
					httpmock.JSONResponse(struct{ Content string }{
						Content: encodedYAMLContent,
					}))
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusStringResponse(204, "cool"))
			},
			promptStubs: func(pm *prompter.MockPrompter) {
				pm.RegisterSelect("Select a workflow", []string{"a workflow (workflow.yml)"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
				pm.RegisterInput("greeting", func(_, _ string) (string, error) {
					return "hi", nil
				})
				pm.RegisterInput("name (required)", func(_, _ string) (string, error) {
					return "scully", nil
				})
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name":     "scully",
					"greeting": "hi",
				},
				"ref": "trunk",
			},
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at trunk

				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			name: "prompt with run info",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{
							{
								Name:  "a workflow",
								ID:    12345,
								State: shared.Active,
								Path:  ".github/workflows/workflow.yml",
							},
						},
					}))
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
					httpmock.JSONResponse(struct{ Content string }{
						Content: encodedYAMLContent,
					}))
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusJSONResponse(200, map[string]any{
						"workflow_run_id": int64(6789),
						"run_url":         "https://api.github.com/repos/OWNER/REPO/actions/runs/6789",
						"html_url":        "https://github.com/OWNER/REPO/actions/runs/6789",
					}))
			},
			promptStubs: func(pm *prompter.MockPrompter) {
				pm.RegisterSelect("Select a workflow", []string{"a workflow (workflow.yml)"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
				pm.RegisterInput("greeting", func(_, _ string) (string, error) {
					return "hi", nil
				})
				pm.RegisterInput("name (required)", func(_, _ string) (string, error) {
					return "scully", nil
				})
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name":     "scully",
					"greeting": "hi",
				},
				"ref":                "trunk",
				"return_run_details": true,
			},
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at trunk
				https://github.com/OWNER/REPO/actions/runs/6789

				To see the created workflow run, try: gh run view 6789
				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			// TODO workflowDispatchRunDetailsCleanup
			// To be deleted
			name: "prompt, workflow choice input without run info (204)",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.DisabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{
							{
								Name:  "choice inputs",
								ID:    12345,
								State: shared.Active,
								Path:  ".github/workflows/workflow.yml",
							},
						},
					}))
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
					httpmock.JSONResponse(struct{ Content string }{
						Content: encodedYAMLContentChoiceIp,
					}))
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusStringResponse(204, "cool"))
			},
			promptStubs: func(pm *prompter.MockPrompter) {
				pm.RegisterSelect("Select a workflow", []string{"choice inputs (workflow.yml)"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
				pm.RegisterSelect("favourite-animal (required)", []string{"dog", "cat"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
				pm.RegisterSelect("name", []string{"monalisa", "cschleiden"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})

			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name":             "monalisa",
					"favourite-animal": "dog",
				},
				"ref": "trunk",
			},
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at trunk

				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			name: "prompt, workflow choice input with run info",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{
							{
								Name:  "choice inputs",
								ID:    12345,
								State: shared.Active,
								Path:  ".github/workflows/workflow.yml",
							},
						},
					}))
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
					httpmock.JSONResponse(struct{ Content string }{
						Content: encodedYAMLContentChoiceIp,
					}))
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusJSONResponse(200, map[string]any{
						"workflow_run_id": int64(6789),
						"run_url":         "https://api.github.com/repos/OWNER/REPO/actions/runs/6789",
						"html_url":        "https://github.com/OWNER/REPO/actions/runs/6789",
					}))
			},
			promptStubs: func(pm *prompter.MockPrompter) {
				pm.RegisterSelect("Select a workflow", []string{"choice inputs (workflow.yml)"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
				pm.RegisterSelect("favourite-animal (required)", []string{"dog", "cat"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
				pm.RegisterSelect("name", []string{"monalisa", "cschleiden"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})

			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name":             "monalisa",
					"favourite-animal": "dog",
				},
				"ref":                "trunk",
				"return_run_details": true,
			},
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at trunk
				https://github.com/OWNER/REPO/actions/runs/6789

				To see the created workflow run, try: gh run view 6789
				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			name: "prompt, workflow choice missing input",
			tty:  true,
			opts: &RunOptions{
				Prompt:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/actions/workflows"),
					httpmock.JSONResponse(shared.WorkflowsPayload{
						Workflows: []shared.Workflow{
							{
								Name:  "choice missing inputs",
								ID:    12345,
								State: shared.Active,
								Path:  ".github/workflows/workflow.yml",
							},
						},
					}))
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
					httpmock.JSONResponse(struct{ Content string }{
						Content: encodedYAMLContentMissingChoiceIp,
					}))
			},
			promptStubs: func(pm *prompter.MockPrompter) {
				pm.RegisterSelect("Select a workflow", []string{"choice missing inputs (workflow.yml)"}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
				pm.RegisterSelect("name", []string{}, func(_, _ string, opts []string) (int, error) {
					return 0, nil
				})
			},
			wantErr: true,
			errOut:  "workflow input \"name\" is of type choice, but has no options",
		},
		{
			name: "undeclared input blocks dispatch",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"nme":"scully"}`,
				Detector:  &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedReconcilableYAMLContent)
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut: `cannot dispatch workflow.yml at trunk: provided inputs do not match its workflow_dispatch declaration:
- undeclared input "nme" is not accepted by workflow.yml (declared inputs: greeting, name)`,
		},
		{
			name: "invalid choice blocks dispatch",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				RawFields: []string{"env=qa"},
				Detector:  &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedChoiceYAMLContent)
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut: `cannot dispatch workflow.yml at trunk: provided inputs do not match its workflow_dispatch declaration:
- value "qa" for choice input "env" is not one of the allowed values: dev, prod`,
		},
		{
			name: "duplicate input from flags and stdin blocks dispatch",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				RawFields: []string{"name=x"},
				JSONInput: `{"name":"y"}`,
				Detector:  &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedReconcilableYAMLContent)
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut: `cannot dispatch workflow.yml at trunk: provided inputs do not match its workflow_dispatch declaration:
- input "name" was provided more than once: from command-line flags the value was "x"; from JSON on standard input the value was "y", and it is unclear which value should be submitted`,
		},
		{
			name: "flags and stdin inputs are merged for dispatch",
			opts: &RunOptions{
				Selector:    "workflow.yml",
				RawFields:   []string{"greeting=hey"},
				JSONInput:   `{"name":"scully"}`,
				Detector:    &fd.EnabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"greeting": "hey",
					"name":     "scully",
				},
				"ref":                "trunk",
				"return_run_details": true,
			},
			httpStubs: stubsWithRunInfo,
			wantOut:   "https://github.com/OWNER/REPO/actions/runs/6789\n",
		},
		{
			name: "valid choice dispatches unchanged",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				RawFields: []string{"env=prod"},
				Detector:  &fd.EnabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs":             map[string]any{"env": "prod"},
				"ref":                "trunk",
				"return_run_details": true,
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedChoiceYAMLContent)
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusJSONResponse(200, map[string]any{
						"workflow_run_id": int64(6789),
						"html_url":        "https://github.com/OWNER/REPO/actions/runs/6789",
					}))
			},
			wantOut: "https://github.com/OWNER/REPO/actions/runs/6789\n",
		},
		{
			// Defaults stay server-side: the submitted inputs must be exactly
			// what the user provided.
			name: "declared defaults are not injected into dispatch",
			tty:  true,
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Detector:  &fd.DisabledDetectorMock{},
			},
			wantBody: map[string]any{
				"inputs": map[string]any{
					"name": "scully",
				},
				"ref": "trunk",
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedYAMLContent)
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusStringResponse(204, "cool"))
			},
			wantOut: heredoc.Doc(`
				✓ Created workflow_dispatch event for workflow.yml at trunk

				To see runs for this workflow, try: gh run list --workflow="workflow.yml"
			`),
		},
		{
			name: "dry run ok prints conclusion without dispatch",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				DryRun:    true,
				Detector:  &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedReconcilableYAMLContent)
			},
			wantNoDispatch: true,
			wantOut: heredoc.Doc(`
				Input reconciliation for workflow.yml at trunk

				Declared inputs:
				- greeting (string, optional)
				- name (string, optional)

				Provided inputs:
				- name=scully (from JSON on standard input)

				Result: all provided inputs match the workflow declaration; nothing was submitted.
			`),
		},
		{
			name: "dry run mismatch exits non-zero without dispatch",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"greeting":"hello there"}`,
				DryRun:    true,
				Detector:  &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedYAMLContent)
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut:         cmdutil.SilentError.Error(),
			wantOut: heredoc.Doc(`
				Input reconciliation for workflow.yml at trunk

				Declared inputs:
				- greeting (string, optional; default: hi) - a greeting
				- name (string, required; missing value) - a name

				Provided inputs:
				- greeting=hello there (from JSON on standard input)

				Inputs not provided; declared defaults will be used:
				- greeting (default: hi)

				Problems:
				- required input "name" was not provided and declares no default

				Result: reconciliation found 1 problem(s); nothing was submitted.
			`),
		},
		{
			name: "dry run json ok",
			opts: &RunOptions{
				Selector:      "workflow.yml",
				JSONInput:     `{"name":"scully"}`,
				DryRun:        true,
				DryRunFormat:  "json",
				Detector:      &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedReconcilableYAMLContent)
			},
			wantNoDispatch: true,
			wantOut: heredoc.Doc(`
				{
				  "workflow": "workflow.yml",
				  "ref": "trunk",
				  "status": "ok",
				  "complete": true,
				  "declared_inputs": [
				    {
				      "name": "greeting",
				      "type": "string",
				      "required": false,
				      "has_default": false,
				      "options": [],
				      "status": "optional_unset",
				      "provided": []
				    },
				    {
				      "name": "name",
				      "type": "string",
				      "required": false,
				      "has_default": false,
				      "options": [],
				      "status": "provided",
				      "provided": [
				        {
				          "value": "scully",
				          "source": "stdin"
				        }
				      ]
				    }
				  ],
				  "provided_inputs": [
				    {
				      "name": "name",
				      "declared": true,
				      "status": "ok",
				      "options": [],
				      "values": [
				        {
				          "value": "scully",
				          "source": "stdin"
				        }
				      ]
				    }
				  ],
				  "defaulted_inputs": [],
				  "findings": []
				}
			`),
		},
		{
			name: "dry run says no inputs are needed",
			opts: &RunOptions{
				Selector: "workflow.yml",
				DryRun:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedNoInputsYAMLContent)
			},
			wantNoDispatch: true,
			wantOut: heredoc.Doc(`
				Input reconciliation for workflow.yml at trunk

				workflow.yml declares no workflow_dispatch inputs; none are needed to trigger it.

				Result: all provided inputs match the workflow declaration; nothing was submitted.
			`),
		},
		{
			name: "dry run without inputs reports missing required inputs",
			opts: &RunOptions{
				Selector: "workflow.yml",
				DryRun:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedYAMLContent)
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut:         cmdutil.SilentError.Error(),
			wantOut: heredoc.Doc(`
				Input reconciliation for workflow.yml at trunk

				Declared inputs:
				- greeting (string, optional; default: hi) - a greeting
				- name (string, required; missing value) - a name

				Inputs not provided; declared defaults will be used:
				- greeting (default: hi)

				Problems:
				- required input "name" was not provided and declares no default

				Result: reconciliation found 1 problem(s); nothing was submitted.
			`),
		},
		{
			name: "missing definition stops when policy is stop",
			opts: &RunOptions{
				Selector:          "workflow.yml",
				JSONInput:         `{"name":"scully"}`,
				OnDefinitionError: definitionErrorStop,
				Detector:          &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
					httpmock.StatusStringResponse(404, "not found"))
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut:         `cannot dispatch workflow.yml at trunk: input reconciliation could not be completed: the workflow file ".github/workflows/workflow.yml" could not be found on ref "trunk"`,
		},
		{
			name: "missing definition proceeds with warning by default",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Detector:  &fd.DisabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
					httpmock.StatusStringResponse(404, "not found"))
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusStringResponse(204, "cool"))
			},
			wantBody: map[string]any{
				"inputs": map[string]any{"name": "scully"},
				"ref":    "trunk",
			},
			wantErrOut: "warning: input reconciliation for workflow.yml at trunk could not be completed: " +
				`the workflow file ".github/workflows/workflow.yml" could not be found on ref "trunk"; submitting inputs unchanged` + "\n",
		},
		{
			name: "non-dispatch workflow stops when policy is stop",
			opts: &RunOptions{
				Selector:          "workflow.yml",
				JSONInput:         `{"name":"scully"}`,
				OnDefinitionError: definitionErrorStop,
				Detector:          &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedNotDispatchYAMLContent)
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut:         `cannot dispatch workflow.yml at trunk: input reconciliation could not be completed: the workflow file does not declare a workflow_dispatch event, so it cannot be manually triggered`,
		},
		{
			name: "malformed definition proceeds with warning and unchanged inputs",
			opts: &RunOptions{
				Selector:  "workflow.yml",
				JSONInput: `{"name":"scully"}`,
				Detector:  &fd.DisabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				registerContents(reg, encodedMalformedYAMLContent)
				reg.Register(
					httpmock.REST("POST", "repos/OWNER/REPO/actions/workflows/12345/dispatches"),
					httpmock.StatusStringResponse(204, "cool"))
			},
			wantBody: map[string]any{
				"inputs": map[string]any{"name": "scully"},
				"ref":    "trunk",
			},
			wantErrOut: "warning: input reconciliation for workflow.yml at trunk could not be completed: " +
				"could not parse the workflow definition: could not decode workflow inputs: yaml: unmarshal errors:\n" +
				"  line 4: cannot unmarshal !!str `lol` into map[string]run.WorkflowInput; submitting inputs unchanged\n",
		},
		{
			name: "dry run reports incomplete definition without dispatch",
			opts: &RunOptions{
				Selector: "workflow.yml",
				DryRun:   true,
				Detector: &fd.EnabledDetectorMock{},
			},
			httpStubs: func(reg *httpmock.Registry) {
				registerWorkflow(reg)
				reg.Register(
					httpmock.REST("GET", "repos/OWNER/REPO/contents/.github%2Fworkflows%2Fworkflow.yml"),
					httpmock.StatusStringResponse(404, "not found"))
			},
			wantNoDispatch: true,
			wantErr:        true,
			errOut:         cmdutil.SilentError.Error(),
			wantOut: heredoc.Doc(`
				Input reconciliation for workflow.yml at trunk

				Reconciliation status: incomplete
				Reason: the workflow file ".github/workflows/workflow.yml" could not be found on ref "trunk"

				Result: inputs were not reconciled; nothing was submitted.
			`),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reg := &httpmock.Registry{}
			defer reg.Verify(t)
			if tt.httpStubs != nil {
				tt.httpStubs(reg)
			}
			tt.opts.HttpClient = func() (*http.Client, error) {
				return &http.Client{Transport: reg}, nil
			}

			ios, _, stdout, stderr := iostreams.Test()
			ios.SetStdinTTY(tt.tty)
			ios.SetStdoutTTY(tt.tty)
			tt.opts.IO = ios
			tt.opts.BaseRepo = func() (ghrepo.Interface, error) {
				return api.InitRepoHostname(&api.Repository{
					Name:             "REPO",
					Owner:            api.RepositoryOwner{Login: "OWNER"},
					DefaultBranchRef: api.BranchRef{Name: "trunk"},
				}, "github.com"), nil
			}

			pm := prompter.NewMockPrompter(t)
			tt.opts.Prompter = pm
			if tt.promptStubs != nil {
				tt.promptStubs(pm)
			}

			assertNoDispatch := func() {
				for _, req := range reg.Requests {
					assert.NotEqual(t, "POST", req.Method, "no dispatch event should be created")
				}
			}

			err := runRun(tt.opts)
			if tt.wantErr {
				assert.Error(t, err)
				assert.Equal(t, tt.errOut, err.Error())
				if tt.wantNoDispatch {
					assertNoDispatch()
				}
				assert.Equal(t, tt.wantErrOut, stderr.String())
				return
			}
			assert.NoError(t, err)
			assert.Equal(t, tt.wantOut, stdout.String())
			assert.Equal(t, tt.wantErrOut, stderr.String())
			if tt.wantNoDispatch {
				assertNoDispatch()
			}

			if len(reg.Requests) > 0 {
				lastRequest := reg.Requests[len(reg.Requests)-1]
				if lastRequest.Method == "POST" {
					bodyBytes, _ := io.ReadAll(lastRequest.Body)
					reqBody := make(map[string]any)
					err := json.Unmarshal(bodyBytes, &reqBody)
					if err != nil {
						t.Fatalf("error decoding JSON: %v", err)
					}
					assert.Equal(t, tt.wantBody, reqBody)
				}
			}
		})
	}
}
