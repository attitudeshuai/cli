package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/MakeNowJust/heredoc"
	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/pkg/cmd/workflow/shared"
	"github.com/cli/cli/v2/pkg/iostreams"
	ghAPI "github.com/cli/go-gh/v2/pkg/api"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func flagInput(name, value string) inputProvision {
	return inputProvision{Name: name, Value: value, Source: sourceFlag}
}

func stdinInput(name, value string) inputProvision {
	return inputProvision{Name: name, Value: value, Source: sourceStdin}
}

func findingTypes(findings []finding) []findingType {
	out := make([]findingType, 0, len(findings))
	for _, f := range findings {
		out = append(out, f.Type)
	}
	return out
}

func TestReconcileInputs(t *testing.T) {
	choiceDecl := WorkflowInput{Name: "env", Required: true, Type: "choice", Options: []string{"dev", "stage", "prod"}}
	greetingDecl := WorkflowInput{Name: "greeting", Default: "hi"}
	nameDecl := WorkflowInput{Name: "name", Required: true}

	tests := []struct {
		name               string
		declared           []WorkflowInput
		provisions         []inputProvision
		wantComplete       bool
		wantStatuses       map[string]string
		wantDefaulted      []string
		wantFindingTypes   []findingType
		wantProvidedStatus map[string]findingType
	}{
		{
			name:         "empty declaration needs no inputs",
			declared:     []WorkflowInput{},
			provisions:   nil,
			wantComplete: true,
		},
		{
			name:         "all inputs provided",
			declared:     []WorkflowInput{greetingDecl, nameDecl},
			provisions:   []inputProvision{flagInput("greeting", "hey"), flagInput("name", "scully")},
			wantComplete: true,
			wantStatuses: map[string]string{
				"greeting": statusProvided,
				"name":     statusProvided,
			},
			wantProvidedStatus: map[string]findingType{
				"greeting": findingOK,
				"name":     findingOK,
			},
		},
		{
			name:         "unprovided input falls back to default",
			declared:     []WorkflowInput{greetingDecl, nameDecl},
			provisions:   []inputProvision{flagInput("name", "scully")},
			wantComplete: true,
			wantStatuses: map[string]string{
				"greeting": statusWillUseDefault,
				"name":     statusProvided,
			},
			wantDefaulted: []string{"greeting"},
		},
		{
			name:         "optional input without default is left unset",
			declared:     []WorkflowInput{{Name: "note"}},
			provisions:   nil,
			wantComplete: true,
			wantStatuses: map[string]string{"note": statusOptionalUnset},
		},
		{
			name:             "missing required input without default",
			declared:         []WorkflowInput{nameDecl},
			provisions:       nil,
			wantComplete:     true,
			wantStatuses:     map[string]string{"name": statusMissingRequired},
			wantFindingTypes: []findingType{findingMissing},
		},
		{
			name:          "required input with a default is not missing",
			declared:      []WorkflowInput{{Name: "env", Required: true, Default: "dev"}},
			provisions:    nil,
			wantComplete:  true,
			wantStatuses:  map[string]string{"env": statusWillUseDefault},
			wantDefaulted: []string{"env"},
		},
		{
			name:             "undeclared input with declarations listed",
			declared:         []WorkflowInput{{Name: "name"}},
			provisions:       []inputProvision{stdinInput("nme", "scully")},
			wantComplete:     true,
			wantStatuses:     map[string]string{"name": statusOptionalUnset},
			wantFindingTypes: []findingType{findingUndeclared},
			wantProvidedStatus: map[string]findingType{
				"nme": findingUndeclared,
			},
		},
		{
			name:             "undeclared input when the workflow declares none",
			declared:         []WorkflowInput{},
			provisions:       []inputProvision{flagInput("anything", "x")},
			wantComplete:     true,
			wantFindingTypes: []findingType{findingUndeclared},
		},
		{
			name:         "choice value within the allowed set",
			declared:     []WorkflowInput{choiceDecl},
			provisions:   []inputProvision{flagInput("env", "stage")},
			wantComplete: true,
			wantStatuses: map[string]string{"env": statusProvided},
		},
		{
			name:             "choice value outside the allowed set",
			declared:         []WorkflowInput{choiceDecl},
			provisions:       []inputProvision{flagInput("env", "qa")},
			wantComplete:     true,
			wantFindingTypes: []findingType{findingInvalidChoice},
			wantProvidedStatus: map[string]findingType{
				"env": findingInvalidChoice,
			},
		},
		{
			name:             "same input from flags and standard input is a duplicate",
			declared:         []WorkflowInput{nameDecl},
			provisions:       []inputProvision{flagInput("name", "x"), stdinInput("name", "y")},
			wantComplete:     true,
			wantFindingTypes: []findingType{findingDuplicate},
			wantProvidedStatus: map[string]findingType{
				"name": findingDuplicate,
			},
		},
		{
			name:             "findings keep declaration then provision ordering",
			declared:         []WorkflowInput{choiceDecl, greetingDecl, nameDecl},
			provisions:       []inputProvision{flagInput("env", "stage"), stdinInput("nme", "scully")},
			wantComplete:     true,
			wantFindingTypes: []findingType{findingMissing, findingUndeclared},
			wantDefaulted:    []string{"greeting"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := reconcileInputs("workflow.yml", "trunk", tt.declared, tt.provisions)

			require.Equal(t, tt.wantComplete, report.Complete)

			if tt.wantStatuses != nil {
				require.Len(t, report.Statuses, len(tt.declared))
				for _, s := range report.Statuses {
					want, ok := tt.wantStatuses[s.Decl.Name]
					require.True(t, ok, "unexpected declared input %q", s.Decl.Name)
					assert.Equal(t, want, s.Status)
				}
			}

			defaults := report.defaultedInputs()
			if tt.wantDefaulted == nil {
				assert.Empty(t, defaults)
			} else {
				defaultNames := make([]string, 0, len(defaults))
				for _, d := range defaults {
					defaultNames = append(defaultNames, d.Name)
				}
				assert.Equal(t, tt.wantDefaulted, defaultNames)
			}

			if tt.wantFindingTypes == nil {
				assert.Empty(t, report.Findings)
			} else {
				assert.Equal(t, tt.wantFindingTypes, findingTypes(report.Findings))
			}

			if tt.wantProvidedStatus != nil {
				require.Len(t, report.Provided, len(tt.wantProvidedStatus))
				for _, p := range report.Provided {
					want, ok := tt.wantProvidedStatus[p.Name]
					require.True(t, ok, "unexpected provided input %q", p.Name)
					assert.Equal(t, want, p.Status)
				}
			}
		})
	}
}

func TestReconcileFindingMessages(t *testing.T) {
	tests := []struct {
		name        string
		declared    []WorkflowInput
		provisions  []inputProvision
		wantMessage string
		wantOptions []string
		wantValues  []inputProvision
	}{
		{
			name:        "missing required",
			declared:    []WorkflowInput{{Name: "name", Required: true}},
			provisions:  nil,
			wantMessage: `required input "name" was not provided and declares no default`,
		},
		{
			name:        "undeclared lists declared names",
			declared:    []WorkflowInput{{Name: "name"}},
			provisions:  []inputProvision{flagInput("nme", "x")},
			wantMessage: `undeclared input "nme" is not accepted by workflow.yml (declared inputs: name)`,
		},
		{
			name:        "undeclared against empty declaration",
			declared:    []WorkflowInput{},
			provisions:  []inputProvision{flagInput("name", "x")},
			wantMessage: `undeclared input "name" is not accepted by workflow.yml, which declares no inputs`,
		},
		{
			name:        "invalid choice lists options",
			declared:    []WorkflowInput{{Name: "env", Type: "choice", Options: []string{"dev", "prod"}}},
			provisions:  []inputProvision{flagInput("env", "qa")},
			wantMessage: `value "qa" for choice input "env" is not one of the allowed values: dev, prod`,
			wantOptions: []string{"dev", "prod"},
		},
		{
			name:       "duplicate names both sources and values",
			declared:   []WorkflowInput{{Name: "name"}},
			provisions: []inputProvision{flagInput("name", "x"), stdinInput("name", "y")},
			wantMessage: `input "name" was provided more than once: from command-line flags the value was "x"; ` +
				`from JSON on standard input the value was "y", and it is unclear which value should be submitted`,
			wantValues: []inputProvision{flagInput("name", "x"), stdinInput("name", "y")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report := reconcileInputs("workflow.yml", "trunk", tt.declared, tt.provisions)
			require.Len(t, report.Findings, 1)
			f := report.Findings[0]
			assert.Equal(t, tt.wantMessage, f.Message)
			if tt.wantOptions != nil {
				assert.Equal(t, tt.wantOptions, f.Options)
			}
			if tt.wantValues != nil {
				assert.Equal(t, tt.wantValues, f.Values)
			}
		})
	}
}

func TestWriteHumanReport(t *testing.T) {
	declared := []WorkflowInput{
		{Name: "env", Required: true, Type: "choice", Options: []string{"dev", "stage", "prod"}},
		{Name: "greeting", Default: "hi"},
		{Name: "name", Required: true},
	}
	provisions := []inputProvision{flagInput("env", "stage"), stdinInput("nme", "scully")}

	t.Run("mismatch", func(t *testing.T) {
		report := reconcileInputs("workflow.yml", "trunk", declared, provisions)
		var buf bytes.Buffer
		writeHumanReport(&buf, report)
		assert.Equal(t, heredoc.Doc(`
			Input reconciliation for workflow.yml at trunk

			Declared inputs:
			- env (choice, required; allowed values: dev, stage, prod)
			- greeting (string, optional; default: hi)
			- name (string, required; missing value)

			Provided inputs:
			- env=stage (from command-line flags)
			- nme=scully (from JSON on standard input)

			Inputs not provided; declared defaults will be used:
			- greeting (default: hi)

			Problems:
			- required input "name" was not provided and declares no default
			- undeclared input "nme" is not accepted by workflow.yml (declared inputs: env, greeting, name)

			Result: reconciliation found 2 problem(s); nothing was submitted.
		`), buf.String())
	})

	t.Run("ok with defaulted inputs", func(t *testing.T) {
		report := reconcileInputs("workflow.yml", "trunk",
			[]WorkflowInput{{Name: "greeting", Default: "hi"}, {Name: "name"}},
			[]inputProvision{flagInput("name", "scully")})
		var buf bytes.Buffer
		writeHumanReport(&buf, report)
		assert.Equal(t, heredoc.Doc(`
			Input reconciliation for workflow.yml at trunk

			Declared inputs:
			- greeting (string, optional; default: hi)
			- name (string, optional)

			Provided inputs:
			- name=scully (from command-line flags)

			Inputs not provided; declared defaults will be used:
			- greeting (default: hi)

			Result: all provided inputs match the workflow declaration; nothing was submitted.
		`), buf.String())
	})

	t.Run("empty declaration", func(t *testing.T) {
		report := reconcileInputs("workflow.yml", "trunk", []WorkflowInput{}, nil)
		var buf bytes.Buffer
		writeHumanReport(&buf, report)
		assert.Equal(t, heredoc.Doc(`
			Input reconciliation for workflow.yml at trunk

			workflow.yml declares no workflow_dispatch inputs; none are needed to trigger it.

			Result: all provided inputs match the workflow declaration; nothing was submitted.
		`), buf.String())
	})

	t.Run("incomplete", func(t *testing.T) {
		report := incompleteReport("workflow.yml", "feature-x", incompleteNotFound,
			`the workflow file ".github/workflows/workflow.yml" could not be found on ref "feature-x"`)
		var buf bytes.Buffer
		writeHumanReport(&buf, report)
		assert.Equal(t, heredoc.Doc(`
			Input reconciliation for workflow.yml at feature-x

			Reconciliation status: incomplete
			Reason: the workflow file ".github/workflows/workflow.yml" could not be found on ref "feature-x"

			Result: inputs were not reconciled; nothing was submitted.
		`), buf.String())
	})
}

func TestWriteJSONReport(t *testing.T) {
	declared := []WorkflowInput{
		{Name: "env", Required: true, Type: "choice", Options: []string{"dev", "stage", "prod"}},
		{Name: "greeting", Default: "hi"},
		{Name: "name", Required: true},
	}
	provisions := []inputProvision{flagInput("env", "stage"), stdinInput("nme", "scully")}
	report := reconcileInputs("workflow.yml", "trunk", declared, provisions)

	var buf bytes.Buffer
	require.NoError(t, writeJSONReport(&buf, report))

	assert.Equal(t, heredoc.Doc(`
		{
		  "workflow": "workflow.yml",
		  "ref": "trunk",
		  "status": "mismatch",
		  "complete": true,
		  "declared_inputs": [
		    {
		      "name": "env",
		      "type": "choice",
		      "required": true,
		      "has_default": false,
		      "options": [
		        "dev",
		        "stage",
		        "prod"
		      ],
		      "status": "provided",
		      "provided": [
		        {
		          "value": "stage",
		          "source": "flag"
		        }
		      ]
		    },
		    {
		      "name": "greeting",
		      "type": "",
		      "required": false,
		      "has_default": true,
		      "default": "hi",
		      "options": [],
		      "status": "will_use_default",
		      "provided": []
		    },
		    {
		      "name": "name",
		      "type": "",
		      "required": true,
		      "has_default": false,
		      "options": [],
		      "status": "missing_required",
		      "provided": []
		    }
		  ],
		  "provided_inputs": [
		    {
		      "name": "env",
		      "declared": true,
		      "status": "ok",
		      "options": [
		        "dev",
		        "stage",
		        "prod"
		      ],
		      "values": [
		        {
		          "value": "stage",
		          "source": "flag"
		        }
		      ]
		    },
		    {
		      "name": "nme",
		      "declared": false,
		      "status": "undeclared_input",
		      "options": [],
		      "values": [
		        {
		          "value": "scully",
		          "source": "stdin"
		        }
		      ]
		    }
		  ],
		  "defaulted_inputs": [
		    "greeting"
		  ],
		  "findings": [
		    {
		      "type": "missing_required",
		      "input": "name",
		      "message": "required input \"name\" was not provided and declares no default"
		    },
		    {
		      "type": "undeclared_input",
		      "input": "nme",
		      "message": "undeclared input \"nme\" is not accepted by workflow.yml (declared inputs: env, greeting, name)"
		    }
		  ]
		}
	`), buf.String())

	// The output must be valid JSON and consumable per item.
	var decoded map[string]any
	require.NoError(t, json.Unmarshal(buf.Bytes(), &decoded))
	assert.Equal(t, "mismatch", decoded["status"])

	t.Run("incomplete reports reason", func(t *testing.T) {
		report := incompleteReport("workflow.yml", "trunk", incompleteNotDispatch,
			"the workflow file does not declare a workflow_dispatch event, so it cannot be manually triggered")
		var b bytes.Buffer
		require.NoError(t, writeJSONReport(&b, report))
		var out jsonReport
		require.NoError(t, json.Unmarshal(b.Bytes(), &out))
		assert.Equal(t, "incomplete", out.Status)
		assert.False(t, out.Complete)
		assert.Contains(t, out.IncompleteReason, "workflow_dispatch")
		assert.Empty(t, out.Findings)
	})
}

func TestBlockingAndIncompleteSubmitErrors(t *testing.T) {
	report := reconcileInputs("workflow.yml", "trunk",
		[]WorkflowInput{{Name: "name", Required: true}}, nil)
	require.True(t, report.hasFindings())

	wantBlocking := "cannot dispatch workflow.yml at trunk: provided inputs do not match its workflow_dispatch declaration:\n" +
		`- required input "name" was not provided and declares no default`
	assert.Equal(t, wantBlocking, blockingError(report).Error())

	incomplete := incompleteReport("workflow.yml", "trunk", incompleteNotFound,
		`the workflow file ".github/workflows/workflow.yml" could not be found on ref "trunk"`)
	assert.Equal(t,
		`cannot dispatch workflow.yml at trunk: input reconciliation could not be completed: `+
			`the workflow file ".github/workflows/workflow.yml" could not be found on ref "trunk"`,
		incompleteSubmitError(incomplete).Error())
}

func TestClassifyDefinitionError(t *testing.T) {
	workflow := &shared.Workflow{Path: ".github/workflows/workflow.yml"}

	t.Run("404 is a missing definition", func(t *testing.T) {
		err := api.HTTPError{HTTPError: &ghAPI.HTTPError{StatusCode: 404}}
		kind, reason := classifyDefinitionError(err, workflow, "feature-x")
		assert.Equal(t, incompleteNotFound, kind)
		assert.Contains(t, reason, `".github/workflows/workflow.yml"`)
		assert.Contains(t, reason, `"feature-x"`)
	})

	t.Run("other HTTP errors are fetch errors", func(t *testing.T) {
		err := api.HTTPError{HTTPError: &ghAPI.HTTPError{StatusCode: 403}}
		kind, reason := classifyDefinitionError(err, workflow, "trunk")
		assert.Equal(t, incompleteFetch, kind)
		assert.Contains(t, reason, "could not read the workflow definition")
	})

	t.Run("non-HTTP errors are parse errors", func(t *testing.T) {
		kind, reason := classifyDefinitionError(errors.New("failed to decode workflow file: nope"), workflow, "trunk")
		assert.Equal(t, incompleteParse, kind)
		assert.Contains(t, reason, "could not parse the workflow definition")
	})
}

func TestClassifySpecError(t *testing.T) {
	t.Run("not workflow_dispatch", func(t *testing.T) {
		_, err := findInputs([]byte("name: workflow\non: pull_request"))
		require.Error(t, err)
		kind, reason := classifySpecError(err, &shared.Workflow{}, "trunk")
		assert.Equal(t, incompleteNotDispatch, kind)
		assert.Contains(t, reason, "workflow_dispatch")
	})

	t.Run("malformed inputs", func(t *testing.T) {
		_, err := findInputs([]byte("name: workflow\non:\n workflow_dispatch:\n  inputs: lol"))
		require.Error(t, err)
		kind, reason := classifySpecError(err, &shared.Workflow{}, "trunk")
		assert.Equal(t, incompleteParse, kind)
		assert.Contains(t, reason, "could not parse the workflow definition")
	})
}

func TestFlagProvisions(t *testing.T) {
	t.Run("raw then magic precedence and order", func(t *testing.T) {
		ios, _, _, _ := iostreams.Test()
		provisions, err := flagProvisions(RunOptions{
			IO:          ios,
			RawFields:   []string{"z=1", "name=raw"},
			MagicFields: []string{"a=2", "name=magic"},
		})
		require.NoError(t, err)
		assert.Equal(t, []inputProvision{
			{Name: "z", Value: "1", Source: sourceFlag},
			{Name: "name", Value: "magic", Source: sourceFlag},
			{Name: "a", Value: "2", Source: sourceFlag},
		}, provisions)
	})

	t.Run("missing equals sign", func(t *testing.T) {
		ios, _, _, _ := iostreams.Test()
		_, err := flagProvisions(RunOptions{IO: ios, RawFields: []string{"novalue"}})
		require.Error(t, err)
	})
}
