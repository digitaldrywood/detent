package nochange_test

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/hubclient/nochange"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { os.Exit(testsupport.RunMain(m)) }
func TestNoChangeCompletionSuppliesCurrentBaseToRequiredGates(t *testing.T) {
	scenariotest.RunNoChangeCompletion(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}
func TestNoChangeRunnerExecutesIndependentRequiredGates(t *testing.T) {
	scenariotest.RunNoChangeRequiredGates(t, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution})
}

func TestRunnerAbsorbedCompletion(t *testing.T) {
	scenariotest.Run(t, []scenariotest.Scenario{
		{Name: "absorbed Rework settles the original version on its actual base", Absorbed: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "NativeCompletion no-change source already on target", Delivered: "ancestor", AdvanceTarget: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "NativeCompletion no-change reviewed head already delivered", Delivered: "exact", Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "absorbed SSH Rework settles without a second provider session", Absorbed: true, SSH: true, Rework: true, Land: true, WantState: "Done", WantChanges: 1},
		{Name: "absorbed Rework preserves formal human rejection", Absorbed: true, Rework: true, Formal: true, Land: true, WantState: "Human Review", WantChanges: 1},
		{Name: "absorbed Rework preserves a reported human hold", Absorbed: true, Hold: true, Rework: true, Land: true, WantState: "Human Review", WantChanges: 1},
	}, scenariotest.Constructors{New: nativechange.NewExecution, Recovered: nativechange.NewRecoveredExecution}, scenariotest.Checks{})
}

func TestPrepareRejectsMalformedReportedSHA(t *testing.T) {
	base := strings.Repeat("a", 40)
	for _, tt := range []struct {
		name, sha, evidence string
		valid               bool
	}{
		{"first colliding tuple", "x", "y:z", false},
		{"second colliding tuple", "x:y", "z", false},
		{"nonhex full length", strings.Repeat("g", 40), "evidence", false},
		{"full commit ID", base, "evidence", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			message := fmt.Sprintf("```detent-status\nschema: 1\nstatus: no_change_needed\nfields:\n  completion_verified_sha: %q\n  completion_evidence: %q\n```", tt.sha, tt.evidence)
			change := nochange.Prepare(tracker.AttemptDiffRequest{BaseSHA: base}, tracker.ChangeDetail{}, nochange.State{Message: message, Worktree: "clean", Repository: "repository", PolicyID: "policy"})
			if (change != nil) != tt.valid {
				t.Fatalf("prepared change = %+v; want valid %v", change, tt.valid)
			}
		})
	}
}
