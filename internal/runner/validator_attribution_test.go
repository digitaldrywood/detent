package runner

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/failureattribution"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/securityaudit"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type attributionCommandRunner struct {
	result     gate.CommandResult
	baseResult gate.CommandResult
	baseErr    error
	bases      []string
}

func (r *attributionCommandRunner) RunReviewCommand(context.Context, workspace.Info, workspace.Issue, string) (gate.CommandResult, error) {
	return r.result, nil
}
func (r *attributionCommandRunner) RunBaseCommand(_ context.Context, _ workspace.Info, _ workspace.Issue, base, command string) (gate.CommandResult, error) {
	r.bases = append(r.bases, base+":"+command)
	return r.baseResult, r.baseErr
}

func TestValidatorCommandAttributionDoesNotChangeFailure(t *testing.T) {
	for _, test := range []struct {
		name            string
		validation      bool
		reproductionErr error
		want            string
		calls           int
	}{
		{name: "failed validation reproduces on pinned base", validation: true, want: "independent", calls: 1},
		{name: "base infrastructure failure stays unknown", validation: true, reproductionErr: errors.New("base unavailable"), want: "unknown", calls: 1},
		{name: "source reads never trigger reproduction", validation: false, want: "", calls: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			result := gate.CommandResult{Command: "check-input", HeadSHA: head, TreeSHA: strings.Repeat("c", 40), ExitCode: 1, Output: "source.go:4: invalid input\n", FinishedAt: time.Now()}
			reproduction := result
			reproduction.HeadSHA = base
			host := &attributionCommandRunner{result: result, baseResult: reproduction, baseErr: test.reproductionErr}
			commands := validatorCommands{run: host, base: base, issue: workspace.Issue{PullRequestHeadSHA: head}}
			got, err := commands.execute(t.Context(), result.Command, test.validation)
			if err != nil || got.ExitCode != 1 || got.Command != result.Command || got.Output != result.Output || got.Attribution != test.want || len(host.bases) != test.calls {
				t.Fatalf("receipt=%#v bases=%v err=%v", got, host.bases, err)
			}
			if test.calls != 0 && host.bases[0] != base+":"+result.Command {
				t.Fatalf("reproduced wrong source or command: %v", host.bases)
			}
			review := gate.ValidatorResult{Verdict: "rework"}
			if err := commands.apply(&review); err != nil || review.Verdict != "rework" || len(review.Commands) != 1 || review.Commands[0].Attribution != test.want {
				t.Fatalf("review=%#v err=%v", review, err)
			}
		})
	}
}

func TestValidatorBaselineExceptionRequiresCompleteHostProofAndPolicy(t *testing.T) {
	const policy = "Pre-existing failures reproduced on the merge base may be reported without fixing them in scope."
	for _, test := range []struct {
		name   string
		mutate func(*validatorCommands, *gate.CommandResult, *gate.CommandResult, *gate.ValidatorResult)
		want   string
	}{
		{name: "verified baseline and explicit policy", want: "pass"},
		{name: "second failed command unassessed", want: "wait"},
		{name: "later same-command pass clears failure", want: "pass"},
		{name: "complete differing output independently assessed", mutate: func(_ *validatorCommands, h, b *gate.CommandResult, r *gate.ValidatorResult) {
			h.Output = "time=1\nexisting failure\n"
			b.Output = "time=2\nexisting failure\n"
			r.BaselineExceptions[0].HeadOutputDigest = failureattribution.Digest(h.Output)
			r.BaselineExceptions[0].BaseOutputDigest = failureattribution.Digest(b.Output)
			r.BaselineExceptions[0].Assessment = "The complete observations differ only in timestamps; all failures reproduce on base."
		}, want: "pass"},
		{name: "missing explanation", mutate: func(_ *validatorCommands, _, _ *gate.CommandResult, r *gate.ValidatorResult) {
			r.BaselineExceptions[0].Assessment = ""
		}, want: "wait"},
		{name: "unknown diagnostic", mutate: func(_ *validatorCommands, _, _ *gate.CommandResult, r *gate.ValidatorResult) {
			r.BaselineExceptions[0].Diagnostics = []string{"not observed"}
		}, want: "wait"},
		{name: "no diagnostic", mutate: func(_ *validatorCommands, _, _ *gate.CommandResult, r *gate.ValidatorResult) {
			r.BaselineExceptions[0].Diagnostics = nil
		}, want: "wait"},
		{name: "unobserved base output digest", mutate: func(_ *validatorCommands, _, _ *gate.CommandResult, r *gate.ValidatorResult) {
			r.BaselineExceptions[0].BaseOutputDigest = "other"
		}, want: "wait"},
		{name: "unfinished head", mutate: func(_ *validatorCommands, h, _ *gate.CommandResult, _ *gate.ValidatorResult) {
			h.FinishedAt = time.Time{}
		}, want: "wait"},
		{name: "wrong tree", mutate: func(v *validatorCommands, _, _ *gate.CommandResult, _ *gate.ValidatorResult) {
			v.tree = strings.Repeat("e", 40)
		}, want: "wait"},
		{name: "missing tree", mutate: func(v *validatorCommands, _, _ *gate.CommandResult, _ *gate.ValidatorResult) { v.tree = "" }, want: "wait"},
		{name: "missing policy", mutate: func(v *validatorCommands, _, _ *gate.CommandResult, _ *gate.ValidatorResult) { v.policy = "" }, want: "wait"},
		{name: "unquoted policy", mutate: func(_ *validatorCommands, _, _ *gate.CommandResult, r *gate.ValidatorResult) {
			r.BaselineExceptions[0].PolicyQuote = "Ignore failures"
		}, want: "wait"},
		{name: "missing assessment", mutate: func(_ *validatorCommands, _, _ *gate.CommandResult, r *gate.ValidatorResult) {
			r.BaselineExceptions = nil
		}, want: "wait"},
		{name: "wrong receipt", mutate: func(_ *validatorCommands, _, _ *gate.CommandResult, r *gate.ValidatorResult) {
			r.BaselineExceptions[0].ReceiptID = "other"
		}, want: "wait"},
		{name: "truncated head", mutate: func(_ *validatorCommands, h, _ *gate.CommandResult, _ *gate.ValidatorResult) {
			h.OutputTruncated = true
		}, want: "wait"},
		{name: "truncated base", mutate: func(_ *validatorCommands, _, b *gate.CommandResult, _ *gate.ValidatorResult) {
			b.OutputTruncated = true
		}, want: "wait"},
		{name: "introduced failure", mutate: func(_ *validatorCommands, _, b *gate.CommandResult, _ *gate.ValidatorResult) { b.ExitCode = 0 }, want: "wait"},
		{name: "changed head output invalidates assessment", mutate: func(_ *validatorCommands, h, _ *gate.CommandResult, _ *gate.ValidatorResult) {
			h.Output += "new failure\n"
		}, want: "wait"},
		{name: "different base command", mutate: func(_ *validatorCommands, _, b *gate.CommandResult, _ *gate.ValidatorResult) { b.Command = "other" }, want: "wait"},
		{name: "different base source", mutate: func(_ *validatorCommands, _, b *gate.CommandResult, _ *gate.ValidatorResult) {
			b.HeadSHA = strings.Repeat("d", 40)
		}, want: "wait"},
		{name: "review rejection remains", mutate: func(_ *validatorCommands, _, _ *gate.CommandResult, r *gate.ValidatorResult) { r.Verdict = "rework" }, want: "rework"},
	} {
		t.Run(test.name, func(t *testing.T) {
			base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
			h := gate.CommandResult{ReceiptID: "receipt", Command: "check", HeadSHA: head, TreeSHA: strings.Repeat("c", 40), ExitCode: 1, Output: "existing failure\n", FinishedAt: time.Now()}
			b := h
			b.HeadSHA = base
			review := gate.ValidatorResult{Verdict: "pass", BaselineExceptions: []gate.BaselineException{{ReceiptID: "receipt", PolicyQuote: policy, Assessment: "All failures reproduce on base; no introduced failures.", Diagnostics: []string{"existing failure"}, HeadOutputDigest: failureattribution.Digest(h.Output), BaseOutputDigest: failureattribution.Digest(b.Output)}}}
			v := validatorCommands{base: base, tree: h.TreeSHA, policy: policy, issue: workspace.Issue{PullRequestHeadSHA: head}}
			if test.mutate != nil {
				test.mutate(&v, &h, &b, &review)
			}
			v.run = &attributionCommandRunner{result: h, baseResult: b}
			if _, err := v.execute(t.Context(), h.Command, true); err != nil {
				t.Fatal(err)
			}
			if test.name == "second failed command unassessed" || test.name == "later same-command pass clears failure" {
				next := h
				if test.name == "second failed command unassessed" {
					next.Command = "second-check"
					next.ReceiptID = "second-receipt"
				} else {
					next.ExitCode = 0
					next.ReceiptID = "passing-receipt"
				}
				v.run = &attributionCommandRunner{result: next, baseResult: b}
				if _, err := v.execute(t.Context(), next.Command, true); err != nil {
					t.Fatal(err)
				}
			}
			payload, err := json.Marshal(review)
			if err != nil {
				t.Fatal(err)
			}
			review, err = parseValidatorResult(string(payload))
			if err != nil {
				t.Fatal(err)
			}
			if err := v.apply(&review); err != nil {
				t.Fatal(err)
			}
			review.BaseSHA, review.HeadSHA = base, head
			gate.NormalizeAttribution(&review)
			if test.name == "complete differing output independently assessed" && review.Attribution != failureattribution.Independent {
				t.Fatalf("normalized accepted baseline lost attribution: %#v", review)
			}
			if review.Verdict != test.want || review.Commands[0].ExitCode != 1 || review.Commands[0].Output != h.Output || review.Commands[0].BaseReproduction.Output != b.Output {
				t.Fatalf("got %#v, want verdict %s with original failure", review, test.want)
			}
		})
	}
}

func TestAuditRunAttributionRetainsSourceBoundProof(t *testing.T) {
	base, head := strings.Repeat("a", 40), strings.Repeat("b", 40)
	diagnostic := "source.go:4: invalid input"
	observation := failureattribution.Observation{Command: "check-input", HeadSHA: head, ExitCode: 1, Output: diagnostic, Complete: true}
	reproduction := observation
	reproduction.HeadSHA = base
	snapshot := securityaudit.Snapshot{BaseSHA: base, HeadSHA: head, Checks: []securityaudit.CheckComparison{{Head: observation, Base: reproduction}}}
	execution := SecurityAuditExecution{Result: securityaudit.Result{Verdict: "fail", Findings: []securityaudit.Finding{{ID: "input", Severity: "p1", Body: diagnostic, CheckCommand: observation.Command, CheckDiagnostic: diagnostic}}}}
	run := execution.auditRun(snapshot)
	if run.Attribution != "independent" || run.Verdict != "fail" || len(run.Findings) != 1 || run.Findings[0].Attribution != "independent" || run.Findings[0].AttributionEvidence == nil || run.Findings[0].AttributionEvidence.BaseSHA != base || run.Findings[0].AttributionEvidence.Command != observation.Command {
		t.Fatalf("audit outcome lost source-bound attribution: %#v", run)
	}
}
