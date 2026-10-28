package runner

import (
	"context"
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
