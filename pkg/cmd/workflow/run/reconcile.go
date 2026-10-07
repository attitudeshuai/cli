package run

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/cli/cli/v2/api"
	"github.com/cli/cli/v2/internal/ghrepo"
	"github.com/cli/cli/v2/pkg/cmd/workflow/shared"
)

// provisionSource records where a provided workflow input came from so that
// the same input supplied from more than one place can be reported instead of
// silently picking a winner.
type provisionSource string

const (
	sourceFlag  provisionSource = "flag"
	sourceStdin provisionSource = "stdin"
)

// humanSourceLabel maps a provision source to the phrase used in reports.
var humanSourceLabel = map[provisionSource]string{
	sourceFlag:  "command-line flags",
	sourceStdin: "JSON on standard input",
}

// inputProvision is a single input supplied for a dispatch, paired with the
// source it came from.
type inputProvision struct {
	Name   string
	Value  string
	Source provisionSource
}

// findingType identifies a single way a provided input set can fail to match
// the inputs a workflow declares.
type findingType string

const (
	findingOK            findingType = "ok"
	findingUndeclared    findingType = "undeclared_input"
	findingMissing       findingType = "missing_required"
	findingInvalidChoice findingType = "invalid_choice"
	findingDuplicate     findingType = "duplicate_input"
)

// Per-input reconciliation statuses, used in both the human and JSON reports.
const (
	// A declared input that was provided for this dispatch.
	statusProvided = "provided"
	// A declared input that was not provided but will use its declared default.
	statusWillUseDefault = "will_use_default"
	// A required declared input with no default that was not provided.
	statusMissingRequired = "missing_required"
	// An optional declared input without a default that was not provided.
	statusOptionalUnset = "optional_unset"
)

// finding describes one mismatch between the provided inputs and the declared
// inputs. Options carries the allowed values for choice-related findings.
type finding struct {
	Type    findingType
	Input   string
	Message string
	Options []string
	Values  []inputProvision
}

// declaredInputStatus pairs a declared input with everything provided for it.
type declaredInputStatus struct {
	Decl     WorkflowInput
	Provided []inputProvision
	Status   string
}

// providedInputStatus pairs a provided input name with every value supplied
// for it and how it relates to the declaration.
type providedInputStatus struct {
	Name     string
	Values   []inputProvision
	Declared bool
	Status   findingType
	Options  []string
}

// incompleteKind says why a definition could not be reconciled against.
type incompleteKind string

const (
	incompleteNotFound    incompleteKind = "not_found"
	incompleteNotDispatch incompleteKind = "not_manual"
	incompleteParse       incompleteKind = "parse_error"
	incompleteFetch       incompleteKind = "fetch_error"
)

// reconciliationReport is the full input reconciliation conclusion for one
// workflow at one ref.
type reconciliationReport struct {
	Workflow         string
	Ref              string
	Complete         bool
	IncompleteKind   incompleteKind
	IncompleteReason string
	Declared         []WorkflowInput
	Statuses         []declaredInputStatus
	Provided         []providedInputStatus
	Findings         []finding
}

// hasFindings reports whether reconciliation found mismatches that the server
// would reject (or that are otherwise unsafe to submit).
func (r *reconciliationReport) hasFindings() bool {
	return len(r.Findings) > 0
}

// defaultedInputs returns the declared inputs that were not provided and whose
// declared default will therefore be used.
func (r *reconciliationReport) defaultedInputs() []WorkflowInput {
	var out []WorkflowInput
	for _, s := range r.Statuses {
		if s.Status == statusWillUseDefault {
			out = append(out, s.Decl)
		}
	}
	return out
}

// reconcileInputs compares provided inputs against the inputs declared by a
// workflow_dispatch trigger. The declarations are expected to come from
// findInputs, which is the same parser used by the interactive prompt path.
// provisions must be in deterministic order: flags first in command-line
// order, then standard input values sorted by name.
func reconcileInputs(workflowName, ref string, declared []WorkflowInput, provisions []inputProvision) *reconciliationReport {
	report := &reconciliationReport{
		Workflow: workflowName,
		Ref:      ref,
		Complete: true,
		Declared: declared,
	}

	byName := map[string]WorkflowInput{}
	for _, input := range declared {
		byName[input.Name] = input
		report.Statuses = append(report.Statuses, declaredInputStatus{Decl: input})
	}

	providedByName := map[string][]inputProvision{}
	var providedOrder []string
	for _, p := range provisions {
		if _, seen := providedByName[p.Name]; !seen {
			providedOrder = append(providedOrder, p.Name)
		}
		providedByName[p.Name] = append(providedByName[p.Name], p)
	}

	for i := range report.Statuses {
		s := &report.Statuses[i]
		provided := providedByName[s.Decl.Name]
		s.Provided = provided
		switch {
		case len(provided) > 0:
			s.Status = statusProvided
		case s.Decl.Required && s.Decl.Default == "":
			s.Status = statusMissingRequired
			report.Findings = append(report.Findings, finding{
				Type:    findingMissing,
				Input:   s.Decl.Name,
				Message: fmt.Sprintf("required input %q was not provided and declares no default", s.Decl.Name),
			})
		case s.Decl.Default != "":
			s.Status = statusWillUseDefault
		default:
			s.Status = statusOptionalUnset
		}

		if s.Decl.Type == "choice" && len(provided) > 0 {
			for _, p := range provided {
				if slices.Contains(s.Decl.Options, p.Value) {
					continue
				}
				report.Findings = append(report.Findings, finding{
					Type:    findingInvalidChoice,
					Input:   s.Decl.Name,
					Options: s.Decl.Options,
					Message: fmt.Sprintf("value %q for choice input %q is not one of the allowed values: %s",
						p.Value, s.Decl.Name, strings.Join(s.Decl.Options, ", ")),
				})
			}
		}
	}

	for _, name := range providedOrder {
		values := providedByName[name]
		status := providedInputStatus{
			Name:   name,
			Values: values,
			Status: findingOK,
		}
		if decl, ok := byName[name]; ok {
			status.Declared = true
			if decl.Type == "choice" {
				status.Options = decl.Options
				for _, p := range values {
					if !slices.Contains(decl.Options, p.Value) {
						status.Status = findingInvalidChoice
						break
					}
				}
			}
		} else {
			var message string
			status.Declared = false
			status.Status = findingUndeclared
			if len(declared) == 0 {
				message = fmt.Sprintf("undeclared input %q is not accepted by %s, which declares no inputs", name, workflowName)
			} else {
				message = fmt.Sprintf("undeclared input %q is not accepted by %s (declared inputs: %s)",
					name, workflowName, joinSortedNames(declared))
			}
			report.Findings = append(report.Findings, finding{
				Type:    findingUndeclared,
				Input:   name,
				Message: message,
			})
		}
		report.Provided = append(report.Provided, status)
	}

	// A name supplied from more than one source is ambiguous and unsafe to
	// submit, so it is reported once with every value and its origin.
	for _, name := range providedOrder {
		values := providedByName[name]
		sources := map[provisionSource]inputProvision{}
		for _, p := range values {
			sources[p.Source] = p
		}
		if len(sources) < 2 {
			continue
		}
		var ordered []inputProvision
		for _, s := range []provisionSource{sourceFlag, sourceStdin} {
			if p, ok := sources[s]; ok {
				ordered = append(ordered, p)
			}
		}
		var b strings.Builder
		fmt.Fprintf(&b, "input %q was provided more than once: ", name)
		for i, p := range ordered {
			if i > 0 {
				b.WriteString("; ")
			}
			fmt.Fprintf(&b, "from %s the value was %q", humanSourceLabel[p.Source], p.Value)
		}
		b.WriteString(", and it is unclear which value should be submitted")
		report.Findings = append(report.Findings, finding{
			Type:    findingDuplicate,
			Input:   name,
			Values:  ordered,
			Message: b.String(),
		})
		for i := range report.Provided {
			if report.Provided[i].Name == name {
				report.Provided[i].Status = findingDuplicate
			}
		}
	}

	return report
}

// incompleteReport builds the conclusion used when the definition cannot be
// fetched or parsed and therefore no declaration comparison is possible.
func incompleteReport(workflowName, ref string, kind incompleteKind, reason string) *reconciliationReport {
	return &reconciliationReport{
		Workflow:         workflowName,
		Ref:              ref,
		Complete:         false,
		IncompleteKind:   kind,
		IncompleteReason: reason,
	}
}

// reconcileWorkflow fetches the workflow definition at the target ref using
// the same endpoint as the interactive prompt path, parses its
// workflow_dispatch inputs, and compares them with the provided inputs. A
// fetch or parse failure yields an incomplete report carrying a precise
// reason instead of an error so callers can apply the configured policy.
func reconcileWorkflow(client *api.Client, repo ghrepo.Interface, workflow *shared.Workflow, ref string, provisions []inputProvision) *reconciliationReport {
	yamlContent, err := shared.GetWorkflowContent(client, repo, *workflow, ref)
	if err != nil {
		kind, reason := classifyDefinitionError(err, workflow, ref)
		return incompleteReport(workflow.Base(), ref, kind, reason)
	}

	declared, err := findInputs(yamlContent)
	if err != nil {
		kind, reason := classifySpecError(err, workflow, ref)
		return incompleteReport(workflow.Base(), ref, kind, reason)
	}

	return reconcileInputs(workflow.Base(), ref, declared, provisions)
}

// classifyDefinitionError maps a failure to read the workflow file onto an
// incompleteness kind with a readable reason.
func classifyDefinitionError(err error, workflow *shared.Workflow, ref string) (incompleteKind, string) {
	if httpErr, ok := errors.AsType[api.HTTPError](err); ok {
		if httpErr.StatusCode == http.StatusNotFound {
			return incompleteNotFound,
				fmt.Sprintf("the workflow file %q could not be found on ref %q", workflow.Path, ref)
		}
		return incompleteFetch, fmt.Sprintf("could not read the workflow definition: %s", err)
	}
	// A non-HTTP error from GetWorkflowContent is a decoding failure, i.e.
	// an unreadable definition rather than a transport problem.
	return incompleteParse, fmt.Sprintf("could not parse the workflow definition: %s", err)
}

// classifySpecError maps a declaration parsing failure onto an
// incompleteness kind with a readable reason.
func classifySpecError(err error, _ *shared.Workflow, _ string) (incompleteKind, string) {
	if specErr, ok := errors.AsType[*specParseError](err); ok && specErr.kind == specNotDispatch {
		return incompleteNotDispatch,
			"the workflow file does not declare a workflow_dispatch event, so it cannot be manually triggered"
	}
	return incompleteParse, fmt.Sprintf("could not parse the workflow definition: %s", err)
}

// sortedInputNames returns the keys of an inputs map in stable order for
// deterministic reports and provision lists.
func sortedInputNames(inputs map[string]string) []string {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func joinSortedNames(declared []WorkflowInput) string {
	names := make([]string, 0, len(declared))
	for _, d := range declared {
		names = append(names, d.Name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// renderFindingsText writes findings as a stable, human-readable bullet list.
func renderFindingsText(w io.Writer, findings []finding) {
	for _, f := range findings {
		fmt.Fprintf(w, "- %s\n", f.Message)
	}
}

// blockingError renders the error returned in submit mode when reconciliation
// finds problems that the server would reject.
func blockingError(r *reconciliationReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "cannot dispatch %s at %s: provided inputs do not match its workflow_dispatch declaration:\n",
		r.Workflow, r.Ref)
	renderFindingsText(&b, r.Findings)
	return fmt.Errorf("%s", strings.TrimRight(b.String(), "\n"))
}

// incompleteSubmitError renders the error used when reconciliation cannot be
// completed and the configured policy is to stop.
func incompleteSubmitError(r *reconciliationReport) error {
	return fmt.Errorf("cannot dispatch %s at %s: input reconciliation could not be completed: %s",
		r.Workflow, r.Ref, r.IncompleteReason)
}

// writeHumanReport prints the full reconciliation conclusion in the
// human-readable format used by --dry-run.
func writeHumanReport(w io.Writer, r *reconciliationReport) {
	fmt.Fprintf(w, "Input reconciliation for %s at %s\n\n", r.Workflow, r.Ref)

	if !r.Complete {
		fmt.Fprintf(w, "Reconciliation status: incomplete\n")
		fmt.Fprintf(w, "Reason: %s\n\n", r.IncompleteReason)
		fmt.Fprintln(w, "Result: inputs were not reconciled; nothing was submitted.")
		return
	}

	if len(r.Declared) == 0 {
		fmt.Fprintf(w, "%s declares no workflow_dispatch inputs; none are needed to trigger it.\n\n", r.Workflow)
	} else {
		fmt.Fprintln(w, "Declared inputs:")
		for _, s := range r.Statuses {
			fmt.Fprintf(w, "- %s\n", describeDeclared(s.Decl, s.Status))
		}
		fmt.Fprintln(w)
	}

	if len(r.Provided) > 0 {
		fmt.Fprintln(w, "Provided inputs:")
		for _, p := range r.Provided {
			for _, v := range p.Values {
				fmt.Fprintf(w, "- %s=%s (from %s)\n", v.Name, v.Value, humanSourceLabel[v.Source])
			}
		}
		fmt.Fprintln(w)
	}

	if defaults := r.defaultedInputs(); len(defaults) > 0 {
		fmt.Fprintln(w, "Inputs not provided; declared defaults will be used:")
		for _, d := range defaults {
			fmt.Fprintf(w, "- %s (default: %s)\n", d.Name, d.Default)
		}
		fmt.Fprintln(w)
	}

	if r.hasFindings() {
		fmt.Fprintln(w, "Problems:")
		renderFindingsText(w, r.Findings)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Result: reconciliation found %d problem(s); nothing was submitted.\n", len(r.Findings))
		return
	}

	fmt.Fprintln(w, "Result: all provided inputs match the workflow declaration; nothing was submitted.")
}

func describeDeclared(d WorkflowInput, status string) string {
	inputType := d.Type
	if inputType == "" {
		inputType = "string"
	}
	required := "optional"
	if d.Required {
		required = "required"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s, %s", d.Name, inputType, required)
	if d.Type == "choice" {
		fmt.Fprintf(&b, "; allowed values: %s", strings.Join(d.Options, ", "))
	}
	if d.Default != "" {
		fmt.Fprintf(&b, "; default: %s", d.Default)
	}
	if status == statusMissingRequired {
		b.WriteString("; missing value")
	}
	b.WriteString(")")
	if d.Description != "" {
		fmt.Fprintf(&b, " - %s", d.Description)
	}
	return b.String()
}

// JSON-serializable report shapes. Every conclusion is emitted per item so the
// output can be consumed by scripts.

type jsonValue struct {
	Value  string `json:"value"`
	Source string `json:"source"`
}

type jsonDeclaredInput struct {
	Name        string      `json:"name"`
	Type        string      `json:"type"`
	Required    bool        `json:"required"`
	HasDefault  bool        `json:"has_default"`
	Default     string      `json:"default,omitempty"`
	Options     []string    `json:"options"`
	Status      string      `json:"status"`
	Provided    []jsonValue `json:"provided"`
	Description string      `json:"description,omitempty"`
}

type jsonProvidedInput struct {
	Name     string      `json:"name"`
	Declared bool        `json:"declared"`
	Status   string      `json:"status"`
	Options  []string    `json:"options"`
	Values   []jsonValue `json:"values"`
}

type jsonFinding struct {
	Type    string      `json:"type"`
	Input   string      `json:"input"`
	Message string      `json:"message"`
	Options []string    `json:"options,omitempty"`
	Values  []jsonValue `json:"values,omitempty"`
}

type jsonReport struct {
	Workflow         string              `json:"workflow"`
	Ref              string              `json:"ref"`
	Status           string              `json:"status"`
	Complete         bool                `json:"complete"`
	IncompleteReason string              `json:"incomplete_reason,omitempty"`
	DeclaredInputs   []jsonDeclaredInput `json:"declared_inputs"`
	ProvidedInputs   []jsonProvidedInput `json:"provided_inputs"`
	DefaultedInputs  []string            `json:"defaulted_inputs"`
	Findings         []jsonFinding       `json:"findings"`
}

func toJSONValues(ps []inputProvision) []jsonValue {
	out := make([]jsonValue, 0, len(ps))
	for _, p := range ps {
		out = append(out, jsonValue{Value: p.Value, Source: string(p.Source)})
	}
	return out
}

func toJSONReport(r *reconciliationReport) jsonReport {
	out := jsonReport{
		Workflow:        r.Workflow,
		Ref:             r.Ref,
		Complete:        r.Complete,
		DeclaredInputs:  []jsonDeclaredInput{},
		ProvidedInputs:  []jsonProvidedInput{},
		DefaultedInputs: []string{},
		Findings:        []jsonFinding{},
	}

	switch {
	case !r.Complete:
		out.Status = "incomplete"
		out.IncompleteReason = r.IncompleteReason
	case r.hasFindings():
		out.Status = "mismatch"
	default:
		out.Status = "ok"
	}

	for _, s := range r.Statuses {
		entry := jsonDeclaredInput{
			Name:        s.Decl.Name,
			Type:        s.Decl.Type,
			Required:    s.Decl.Required,
			HasDefault:  s.Decl.Default != "",
			Default:     s.Decl.Default,
			Options:     s.Decl.Options,
			Status:      s.Status,
			Provided:    toJSONValues(s.Provided),
			Description: s.Decl.Description,
		}
		if entry.Options == nil {
			entry.Options = []string{}
		}
		out.DeclaredInputs = append(out.DeclaredInputs, entry)
	}

	for _, p := range r.Provided {
		entry := jsonProvidedInput{
			Name:     p.Name,
			Declared: p.Declared,
			Status:   string(p.Status),
			Options:  p.Options,
			Values:   toJSONValues(p.Values),
		}
		if entry.Options == nil {
			entry.Options = []string{}
		}
		out.ProvidedInputs = append(out.ProvidedInputs, entry)
	}

	for _, d := range r.defaultedInputs() {
		out.DefaultedInputs = append(out.DefaultedInputs, d.Name)
	}

	for _, f := range r.Findings {
		out.Findings = append(out.Findings, jsonFinding{
			Type:    string(f.Type),
			Input:   f.Input,
			Message: f.Message,
			Options: f.Options,
			Values:  toJSONValues(f.Values),
		})
	}

	return out
}

// writeJSONReport prints the machine-readable, per-item reconciliation
// conclusion used by --dry-run --dry-run-format json.
func writeJSONReport(w io.Writer, r *reconciliationReport) error {
	encoded, err := json.MarshalIndent(toJSONReport(r), "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize input reconciliation: %w", err)
	}
	_, err = fmt.Fprintf(w, "%s\n", encoded)
	return err
}
