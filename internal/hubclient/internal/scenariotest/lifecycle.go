package scenariotest

import (
	"context"
	"encoding/json"

	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"
	executioncontract "github.com/digitaldrywood/detent/internal/hubclient/execution"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Lifecycle interface {
	BindReviewHandoff(<-chan struct{})
	FlushRuntime(context.Context) error
	executioncontract.Execution
	CompletionSnapshot() checkpoint.State
	CompletionState() json.RawMessage
	runner.ArtifactExecution
	runner.ArtifactSourceExecution
	runner.AvailabilityExecution
	runner.ChangeExecution
	runner.ChangeSourceExecution
	runner.CompletionExecution
	runner.ConversationExecution
	runner.DependencyReasonExecution
	runner.DiffExecution
	runner.EvidenceSourceExecution
	runner.HostDiagnosticsExecution
	runner.IntegrationSourceExecution
	runner.LandingExecution
	runner.LandingRuntimeExecution
	runner.MergeResolutionExecution
	runner.NativeReviewContinuationExecution
	runner.NativeReviewExecution
	runner.NativeSecurityAuditExecution
	runner.NativeSourceIterationExecution
	runner.NativeValidatorExecution
	runner.NoChangeExecution
	runner.PhaseReceiptExecution
	runner.RoundVerdictExecution
	runner.PipelineExecution
	runner.PreStartConversationExecution
	runner.ProcessUsageExecution
	runner.ProviderCapacityExecution
	StartPhased(context.Context, tracker.NativeExecutionIdentity) error
	SetPhaseProvider(context.Context, providercapacity.Requirement) error
	runner.PublicationSourceExecution
	runner.ReleaseDeliveryExecution
	runner.RepositoryExecution
	runner.RuntimeExecution
	runner.SourceValidationExecution
	runner.StepExecution
	runner.ToolExecution
	runner.UsageExecution
	runner.ValidationEvidenceExecution
	runner.WorkerLifetimeExecution
}
