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
