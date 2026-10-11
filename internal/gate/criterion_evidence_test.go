package gate

import (
	"reflect"
	"strings"
	"testing"
)

func TestCriterionEvidence(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name       string
		entries    []CriterionEvidence
		commands   []CommandResult
		gaps       []string
		findings   int
		receiptIDs []string
		edit       func(*ValidatorResult)
	}{
		{name: "missing map", gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "named tests", entries: []CriterionEvidence{
			{Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "input_test.go:TestInvalid", Behavior: "Malformed input returns an error"},
			{Criterion: "Return valid input", Kind: EvidenceTest, Reference: "input_test.go:TestValid", Behavior: "Valid input returns its original value"},
		}},
		{name: "restated implementation", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "input_test.go:TestInvalid", Behavior: "Copies parser", RestatesImplementation: true}}, gaps: []string{"Reject invalid input", "Return valid input"}, findings: 1},
		{name: "rejected receipt test", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "go test", RestatesImplementation: true}}, gaps: []string{"Reject invalid input", "Return valid input"}, findings: 1},
		{name: "unexplained test", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "TestInvalid"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "duplicate criterion", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "TestOne", Behavior: "Error"}, {Criterion: "Reject invalid input", Kind: EvidenceTest, Reference: "TestTwo", Behavior: "Error"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "unknown criterion", entries: []CriterionEvidence{{Criterion: "Unrelated", Kind: EvidenceTest, Reference: "TestOne", Behavior: "Error"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "bound receipt", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "head", TreeSHA: "tree", Behavior: "Malformed input returns an error"}}, commands: []CommandResult{{Command: "make check", HeadSHA: "head", TreeSHA: "tree"}}, gaps: []string{"Return valid input"}},
		{name: "missing receipt", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "head", TreeSHA: "tree", Behavior: "Malformed input returns an error"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "failed receipt", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "head", TreeSHA: "tree", Behavior: "Malformed input returns an error"}}, commands: []CommandResult{{Command: "make check", HeadSHA: "head", TreeSHA: "tree", ExitCode: 1}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "wrong head", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "old", TreeSHA: "tree", Behavior: "Malformed input returns an error"}}, commands: []CommandResult{{Command: "make check", HeadSHA: "old", TreeSHA: "tree"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "wrong tree", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check", HeadSHA: "head", TreeSHA: "other", Behavior: "Malformed input returns an error"}}, commands: []CommandResult{{Command: "make check", HeadSHA: "head", TreeSHA: "other"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "multiple bound receipts", receiptIDs: []string{"timing_app", "timing_browser"}, gaps: []string{"Return valid input"}},
		{name: "missing selected receipt", receiptIDs: []string{"timing_app", "missing"}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "duplicate selected receipt", receiptIDs: []string{"timing_app", "timing_app"}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "empty selected receipt", receiptIDs: []string{"timing_app", ""}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "ambiguous receipt ID", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands = append(r.Commands, r.Commands[0]) }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "failed selected receipt", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands[1].ExitCode = 1 }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "selected receipt wrong head", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands[1].HeadSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "selected receipt wrong tree", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.Commands[1].TreeSHA = "other" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "receipt IDs retain expected tree binding", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) {
			r.CriteriaEvidence[0].TreeSHA = "other"
			for i := range r.Commands {
				r.Commands[i].TreeSHA = "other"
			}
		}, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "receipt IDs require behavior", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].Behavior = "" }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "receipt IDs reject restated implementation", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].RestatesImplementation = true }, gaps: []string{"Reject invalid input", "Return valid input"}, findings: 1},
		{name: "invalid ID cannot fall back to exact command", receiptIDs: []string{"missing"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].Reference = r.Commands[0].Command }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "legacy combined command is not split", receiptIDs: []string{"timing_app", "timing_browser"}, edit: func(r *ValidatorResult) { r.CriteriaEvidence[0].ReceiptIDs = nil }, gaps: []string{"Reject invalid input", "Return valid input"}},
		{name: "unsupported kind", entries: []CriterionEvidence{{Criterion: "Reject invalid input", Kind: "approval", Reference: "human"}}, gaps: []string{"Reject invalid input", "Return valid input"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := ValidatorResult{HeadSHA: "head", CriteriaEvidence: test.entries, Commands: test.commands}
			if test.receiptIDs != nil {
				result.CriteriaEvidence = []CriterionEvidence{{Criterion: "Reject invalid input", Kind: EvidenceReceipt, Reference: "make check-app; npx playwright test", ReceiptIDs: test.receiptIDs, HeadSHA: "head", TreeSHA: "tree", Behavior: "Both checks reject malformed input"}}
				result.Commands = []CommandResult{{ReceiptID: "timing_app", Command: "make check-app", HeadSHA: "head", TreeSHA: "tree"}, {ReceiptID: "timing_browser", Command: "npx playwright test", HeadSHA: "head", TreeSHA: "tree"}}
			}
			if test.edit != nil {
				test.edit(&result)
			}
			NormalizeCriterionEvidence(&result, []string{"Reject invalid input", "Return valid input"}, "tree")
			if len(result.CriteriaEvidence) != 2 || result.CriteriaEvidence[0].Criterion != "Reject invalid input" || result.CriteriaEvidence[1].Criterion != "Return valid input" || !reflect.DeepEqual(result.NotVerified, test.gaps) || len(result.Findings) != test.findings {
				t.Fatalf("normalized evidence = %+v", result)
			}
		})
	}
}

func TestCriterionEvidencePolicy(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name        string
		policy      string
		verdict     string
		score       float64
		findings    []Finding
		gaps        []string
		wantVerdict string
		wantReason  Reason
	}{
		{name: "default reworks", verdict: "pass", score: .95, gaps: []string{"Reject invalid input"}, wantVerdict: "rework", wantReason: ReasonValidatorRework},
		{name: "disclosure passes", policy: UnverifiedCriteriaDisclose, verdict: "pass", score: .95, gaps: []string{"Reject invalid input"}, wantVerdict: "pass"},
		{name: "fully verified", verdict: "pass", score: .95, wantVerdict: "pass"},
		{name: "preserve validator error", verdict: "error", gaps: []string{"Reject invalid input"}, wantVerdict: "error", wantReason: ReasonValidatorError},
		{name: "disclosure preserves rework", policy: UnverifiedCriteriaDisclose, verdict: "rework", score: .95, gaps: []string{"Reject invalid input"}, wantVerdict: "rework", wantReason: ReasonValidatorRework},
		{name: "disclosure preserves wait", policy: UnverifiedCriteriaDisclose, verdict: "wait", score: .95, gaps: []string{"Reject invalid input"}, wantVerdict: "wait", wantReason: ReasonValidatorWait},
		{name: "disclosure preserves minimum score", policy: UnverifiedCriteriaDisclose, verdict: "pass", score: .5, gaps: []string{"Reject invalid input"}, wantVerdict: "pass", wantReason: ReasonValidatorScoreBelowThreshold},
		{name: "disclosure preserves severity", policy: UnverifiedCriteriaDisclose, verdict: "pass", score: .95, findings: []Finding{{Severity: "p1", Body: "Regression"}}, gaps: []string{"Reject invalid input"}, wantVerdict: "pass", wantReason: ReasonValidatorBlockedSeverity},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := ValidatorConfig{Enabled: true, MinScore: .8, BlockOn: []string{"p1"}, UnverifiedCriteria: test.policy}
			result := ValidatorResult{Submitted: true, Verdict: test.verdict, Score: test.score, Findings: test.findings, NotVerified: test.gaps}
			ApplyCriterionPolicy(cfg, &result)
			decision, blocked := EvaluateValidator(cfg, result)
			if result.Verdict != test.wantVerdict || decision.Reason != test.wantReason || blocked != (test.wantReason != "") || !reflect.DeepEqual(result.NotVerified, test.gaps) {
				t.Fatalf("result=%+v decision=%+v blocked=%t", result, decision, blocked)
			}
			if test.name == "default reworks" && !strings.Contains(result.Summary, "Reject invalid input") {
				t.Fatalf("summary does not name the gap: %s", result.Summary)
			}
		})
	}
}
