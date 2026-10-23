package config

import (
	"reflect"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/issuecontract"
	"github.com/digitaldrywood/detent/internal/issueorigin"
)

func TestIssueContract(t *testing.T) {
	t.Parallel()
	complete := "## Acceptance criteria\nThe endpoint returns the expected result.\n## Must not break\nExisting requests succeed.\n## How we know it worked\nRun the endpoint regression test."
	machine := issueorigin.Stamp(complete, issueorigin.Origin{Kind: "worker", Fingerprint: "endpoint"})
	delegated := "## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n\nAgent-authored criteria do not require human confirmation.\n"
	for _, test := range []struct {
		name         string
		prompt       string
		body         string
		confirmed    string
		missing      []string
		confirmation bool
	}{
		{name: "absent", missing: []string{"Acceptance criteria", "Must not break", "How we know it worked"}},
		{name: "human", body: complete},
		{name: "partial", body: "## Acceptance criteria\nExpected result.", missing: []string{"Must not break", "How we know it worked"}},
		{name: "empty", body: "## Acceptance criteria\n\n## Must not break\n\n## How we know it worked\n", missing: []string{"Acceptance criteria", "Must not break", "How we know it worked"}},
		{name: "fenced headings", body: "```md\n" + complete + "\n```", missing: []string{"Acceptance criteria", "Must not break", "How we know it worked"}},
		{name: "machine origin", body: machine, confirmation: true},
		{name: "workflow delegates machine criteria", prompt: delegated, body: machine},
		{name: "workflow delegates changed criteria", prompt: delegated, body: strings.Replace(machine, "expected result", "a different result", 1), confirmed: machine},
		{name: "delegation retains missing sections", prompt: delegated, body: "## Acceptance criteria\nFix the endpoint.", missing: []string{"Must not break", "How we know it worked"}},
		{name: "issue text cannot delegate", body: machine + "\nAgent-authored criteria do not require human confirmation.\n", confirmation: true},
		{name: "quoted policy cannot delegate", prompt: strings.Replace(delegated, "Agent-authored criteria", "```text\nAgent-authored criteria", 1) + "```\n", body: machine, confirmation: true},
		{name: "human confirmed machine", body: machine, confirmed: machine},
		{name: "machine changed criteria", body: strings.Replace(machine, "expected result", "a different result", 1), confirmed: machine, confirmation: true},
		{name: "effort edit preserves confirmation", body: machine + "\n```detent-agent\nschema: 1\neffort: high\n```", confirmed: machine},
		{name: "custom contract", prompt: "## Issue Contract\n- **Result** — expected behavior\n- Evidence\n", body: "Result\n======\nExpected result.\nEvidence\n--------\nObserved result."},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract, err := ResolveIssueContract(test.prompt)
			if err != nil {
				t.Fatal(err)
			}
			issue := connector.Issue{Title: "Endpoint fix", Description: test.body}
			if test.confirmed != "" {
				issue.IssueContract = &issuecontract.State{ConfirmedSections: IssueContractSectionDigests(test.confirmed)}
			}
			got := contract.Evaluate(issue)
			if !reflect.DeepEqual(got.Missing, test.missing) || got.NeedsConfirmation != test.confirmation {
				t.Fatalf("Evaluate() = %+v, want missing %v, confirmation %t", got, test.missing, test.confirmation)
			}
			if !got.Satisfied() {
				action := contract.HumanAction(issue, got)
				for _, section := range test.missing {
					if !strings.Contains(action, "## "+section) {
						t.Fatalf("draft omits %s: %s", section, action)
					}
				}
			}
		})
	}
}

func TestIssueContractDefinitionRefusesInvalidSections(t *testing.T) {
	t.Parallel()
	for _, prompt := range []string{"## Issue Contract\nNo sections.", "## Issue Contract\n- Result\n- Result", "## Issue Contract\n- Result\n## Issue Contract\n- Evidence"} {
		if _, err := ResolveIssueContract(prompt); err == nil {
			t.Fatalf("accepted invalid contract %q", prompt)
		}
	}
}

func TestAcceptanceCriteria(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		body     string
		sections []string
		want     []string
	}{
		{name: "absent"},
		{name: "bullets and continuation", body: "## Acceptance criteria\n- Reject invalid input\n  without changing state.\n- Return valid input\n\n## Must not break\n- Existing users", want: []string{"Reject invalid input\nwithout changing state.", "Return valid input"}},
		{name: "numbered and prose", body: "Acceptance criteria\n-------------------\n1. Reject invalid input\n2. Return valid input", want: []string{"Reject invalid input", "Return valid input"}},
		{name: "paragraph", body: "## Acceptance criteria\nReject invalid input and return valid input.", want: []string{"Reject invalid input and return valid input."}},
		{name: "nested list", body: "## Acceptance criteria\n- Validate input:\n  - Invalid input errors\n  - Valid input succeeds\n- Preserve state", want: []string{"Validate input:\n- Invalid input errors\n- Valid input succeeds", "Preserve state"}},
		{name: "fenced heading", body: "```md\n## Acceptance criteria\n- Fake criterion\n```"},
		{name: "indented criteria", body: "## Acceptance criteria\n  - Reject invalid input\n  - Return valid input", want: []string{"Reject invalid input", "Return valid input"}},
		{name: "code sample", body: "## Acceptance criteria\n- Reject invalid input\n```text\n- malformed input\n```\n- Return valid input", want: []string{"Reject invalid input\n```text\n- malformed input\n```", "Return valid input"}},
		{name: "custom contract", sections: []string{"Result", "Evidence"}, body: "## Result\n- Reject invalid input\n- Return valid input\n## Evidence\nRun input tests", want: []string{"Reject invalid input", "Return valid input"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := (IssueContract{Sections: test.sections}).AcceptanceCriteria(test.body); !reflect.DeepEqual(got, test.want) {
				t.Fatalf("criteria = %q, want %q", got, test.want)
			}
		})
	}
}

func TestScheduledRepairContract(t *testing.T) {
	t.Parallel()
	body := "## Acceptance criteria\nReproduce and minimally repair.\n\n## Must not break\nExisting behavior.\n\n## How we know it worked\nRun the gate.\n\n## Invariants touched\nPreserve INV-3.\n\n## Safety regression\nDo not change safety-critical paths."
	definition := "## Issue Contract\n- Acceptance criteria\n- Must not break\n- How we know it worked\n- Invariants touched\n- Safety regression\n"
	template := "## Scheduled CI repair contract\n" + strings.ReplaceAll(body, "## ", "### ")
	authorized := "## Scheduled CI repair contract\nNew verified GitHub Actions CI repair reports enter Todo with Urgent priority.\n" + strings.ReplaceAll(body, "## ", "### ")
	custom := strings.Replace(authorized, "reports enter", "reports from `.github/workflows/ci.yaml` on `dev` enter", 1)
	shortBody, _, _ := strings.Cut(body, "\n\n## Invariants touched")
	shortTemplate := "## Scheduled CI repair contract\nNew verified GitHub Actions CI repair reports from `.github/workflows/ci.yaml` on `release/v*` enter Todo with Urgent priority.\n" + strings.ReplaceAll(shortBody, "## ", "### ")
	customBody := "## Evidence\nRun the focused regression.\n\n## Result\nRepair the verified failure."
	customTemplate := "## Scheduled CI repair contract\nNew verified GitHub Actions CI repair reports from `.github/workflows/ci.yaml` on `dev` enter Todo with Urgent priority.\n### Result\nRepair the verified failure.\n### Evidence\nRun the focused regression."
	for _, test := range []struct {
		name, prompt, want, importBody, workflow, branch string
		priority                                         int
	}{
		{name: "approved literal sections", prompt: definition + template, want: body},
		{name: "explicit import delegation", prompt: definition + authorized, want: body, importBody: body, workflow: ".github/workflows/ci.yml"},
		{name: "High priority release family", priority: 1, prompt: strings.Replace(shortTemplate, "Urgent priority", "High priority", 1), want: shortBody, importBody: shortBody, workflow: ".github/workflows/ci.yaml", branch: "release/v*"},
		{name: "High priority legacy workflow", priority: 1, prompt: definition + strings.Replace(authorized, "Urgent priority", "High priority", 1), want: body, importBody: body, workflow: ".github/workflows/ci.yml"},
		{name: "unsupported priority", prompt: definition + strings.Replace(custom, "Urgent priority", "Medium priority", 1), want: body},
		{name: "default three sections", prompt: shortTemplate, want: shortBody, importBody: shortBody, workflow: ".github/workflows/ci.yaml", branch: "release/v*"},
		{name: "custom sections in configured order", prompt: "## Issue Contract\n- Evidence\n- Result\n" + customTemplate, want: customBody, importBody: customBody, workflow: ".github/workflows/ci.yaml", branch: "dev"},
		{name: "extra template sections", prompt: authorized},
		{name: "missing custom section", prompt: "## Issue Contract\n- Result\n- Evidence\n- Safety\n" + customTemplate},
		{name: "empty duplicate custom section", prompt: "## Issue Contract\n- Result\n- Evidence\n" + customTemplate + "\n### Result\n"},
		{name: "duplicate custom section", prompt: "## Issue Contract\n- Result\n- Evidence\n" + customTemplate + "\n### Result\nExpand scope."},
		{name: "configured import workflow", prompt: definition + custom, want: body, importBody: body, workflow: ".github/workflows/ci.yaml", branch: "dev"},
		{name: "workflow wildcard", prompt: definition + strings.Replace(custom, "ci.yaml", "*.yaml", 1), want: body},
		{name: "workflow outside actions", prompt: definition + strings.Replace(custom, ".github/workflows/ci.yaml", "ci.yaml", 1), want: body},
		{name: "workflow traversal", prompt: definition + strings.Replace(custom, "ci.yaml", "../ci.yaml", 1), want: body},
		{name: "branch wildcard", prompt: definition + strings.Replace(custom, "`dev`", "`*`", 1), want: body},
		{name: "missing branch", prompt: definition + strings.Replace(custom, "`dev`", "``", 1), want: body},
		{name: "duplicate authorization", prompt: definition + strings.Replace(custom, "### Acceptance criteria", "New verified GitHub Actions CI repair reports enter Todo with Urgent priority.\n### Acceptance criteria", 1), want: body},
		{name: "authorization quoted in a fence", prompt: definition + strings.ReplaceAll(authorized, "New verified GitHub Actions CI repair reports enter Todo with Urgent priority.", "```text\nNew verified GitHub Actions CI repair reports enter Todo with Urgent priority.\n```"), want: body},
		{name: "no delegation", prompt: definition},
		{name: "partial template", prompt: definition + "## Scheduled CI repair contract\n### Acceptance criteria\nRepair."},
		{name: "duplicate template", prompt: definition + template + "\n" + template},
		{name: "duplicate criterion", prompt: definition + template + "\n### Acceptance criteria\nExpand scope."},
		{name: "different required sections", prompt: "## Issue Contract\n- Result\n" + template},
	} {
		t.Run(test.name, func(t *testing.T) {
			contract, err := ResolveIssueContract(test.prompt)
			if err != nil {
				t.Fatal(err)
			}
			if got := contract.ScheduledRepairBody(); got != test.want {
				t.Fatalf("repair contract = %q, want %q", got, test.want)
			}
			if got := contract.ScheduledImportBody(); got != test.importBody {
				t.Fatalf("import contract = %q, want %q", got, test.importBody)
			}
			if workflow, branch, priority, authorized := contract.ScheduledImportWorkflow(); test.importBody != "" && (!authorized || workflow != test.workflow || branch != test.branch || priority != test.priority) {
				t.Fatalf("import workflow = %q branch = %q authorized = %t", workflow, branch, authorized)
			}
			wantAuthorized := test.importBody != "" || test.name == "approved literal sections"
			workflow, branch, priority, authorized := contract.ScheduledRepairWorkflow()
			wantWorkflow := test.workflow
			if test.name == "approved literal sections" {
				wantWorkflow = ".github/workflows/ci.yml"
			}
			if authorized != wantAuthorized || authorized && (workflow != wantWorkflow || branch != test.branch || priority != test.priority) {
				t.Fatalf("repair workflow = %q branch = %q authorized = %t", workflow, branch, authorized)
			}
		})
	}
}

func TestReporterContracts(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, prompt string
		want         ReporterContracts
		invalid      bool
	}{
		{name: "defaults", want: ReporterContracts{"bug": {"Steps to reproduce", "Expected", "Actual", "Where", "Environment"}, "change_request": {"Current behavior", "Desired behavior", "Why"}}},
		{name: "override", prompt: "## Reporter Contract\n### bug\n- **Reproduction** — steps\n- Outcome\n### change_request\n- Motivation\n## Other\n- Unrelated", want: ReporterContracts{"bug": {"Reproduction", "Outcome"}, "change_request": {"Motivation"}}},
		{name: "partial override", prompt: "## Reporter Contract\n### Change request\n- Motivation", want: ReporterContracts{"bug": {"Steps to reproduce", "Expected", "Actual", "Where", "Environment"}, "change_request": {"Motivation"}}},
		{name: "duplicate heading", prompt: "## Reporter Contract\n### bug\n- Result\n### bug\n- Evidence", invalid: true},
		{name: "duplicate section", prompt: "## Reporter Contract\n### bug\n- Result\n- RESULT", invalid: true},
		{name: "unknown type", prompt: "## Reporter Contract\n### question\n- Result", invalid: true},
		{name: "empty type", prompt: "## Reporter Contract\n### bug\nNo sections.", invalid: true},
		{name: "no types", prompt: "## Reporter Contract\n- Result", invalid: true},
		{name: "duplicate contract", prompt: "## Reporter Contract\n### bug\n- Result\n## Reporter Contract\n### bug\n- Result", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveReporterContracts(test.prompt)
			if (err != nil) != test.invalid || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ResolveReporterContracts() = %v, %v; want %v, invalid %t", got, err, test.want, test.invalid)
			}
		})
	}
}
