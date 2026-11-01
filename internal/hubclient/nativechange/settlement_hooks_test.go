package nativechange

import (
	"context"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func nativeSettlementChecks() scenariotest.SettlementChecks {
	actual := func(execution runner.Execution) *nativeExecution {
		return scenariotest.Underlying(execution).(*nativeExecution)
	}
	return scenariotest.SettlementChecks{
		Observe: func(ctx context.Context, execution runner.Execution) error {
			return actual(execution).append(ctx, "run.observed", "", nil, true)
		},
		SetConversation: func(execution runner.Execution) {
			owner := actual(execution)
			owner.mu.Lock()
			defer owner.mu.Unlock()
			owner.conversationContinuation = true
		},
		DisableBatching: func(execution runner.Execution) { actual(execution).claim.lease.RunDetailBatching = false },
		ReplayPublication: func(ctx context.Context, execution runner.Execution) error {
			owner := actual(execution)
			owner.publication = nil
			return owner.preparePRPublication(ctx, owner.change)
		},
		TamperPublication: func(execution runner.Execution, kind string) {
			owner := actual(execution)
			switch kind {
			case "base":
				owner.publication.BaseRef = "another-target"
			case "branch":
				owner.publication.Branch = "another-branch"
			case "identity":
				owner.publication.External = tracker.ChangeExternalReference{Provider: "github", ID: "8", URL: nativeChangeRepository + "/pull/8"}
			}
		},
		StoreDiff: func(ctx context.Context, execution runner.Execution, sequence int64) error {
			return actual(execution).postDiff(ctx, sequence)
		},
		HasDiffSource: func(execution runner.Execution) bool { return actual(execution).diffSource != nil },
	}
}
