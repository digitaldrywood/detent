package cli

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// sshNativeProbe checks native callbacks in the existing subprocess/SSH worker
// fixture. Real Hub publication and journal replay are covered in hubclient.
type sshNativeProbe struct {
	mu                     sync.Mutex
	guarded, finished      int
	observations           int
	journalRoot, base, log string
	capture                func(context.Context, string, string) (artifact.GitCapture, error)
	diff                   runner.AttemptDiffSource
	lastDiff               tracker.AttemptDiffRequest
	bundle                 artifact.GitCapture
	producerFailure        bool
	outcome, finalMessage  string
	repository             string
}

func (e *sshNativeProbe) Guard(ctx context.Context) (context.Context, func(), error) {
	e.mu.Lock()
	e.guarded++
	e.mu.Unlock()
	return ctx, func() {}, nil
}
func (*sshNativeProbe) Validate(ctx context.Context) error                           { return ctx.Err() }
func (*sshNativeProbe) Start(context.Context, tracker.NativeExecutionIdentity) error { return nil }
func (e *sshNativeProbe) ObserveRuntime(ctx context.Context, _ tracker.NativeRuntimeObservation) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.observations++
	return ctx.Err()
}
func (e *sshNativeProbe) Checkpoint(ctx context.Context, _ tracker.NativeCheckpoint) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if diff, ok := e.diff(ctx); ok {
		e.lastDiff = diff
	}
	return nil
}
func (e *sshNativeProbe) Finish(_ context.Context, outcome string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finished++
	e.outcome = outcome
	return nil
}
func (*sshNativeProbe) Recovery() tracker.NativeRecovery { return tracker.NativeRecovery{} }
func (e *sshNativeProbe) SetArtifactSource(root string, source func(context.Context, string, string) (artifact.GitCapture, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.journalRoot, e.capture = root, source
}
func (e *sshNativeProbe) SetDiffSource(source runner.AttemptDiffSource) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.diff = source
}
func (e *sshNativeProbe) SetRepository(repository string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.repository = repository
}
func (e *sshNativeProbe) PrepareArtifacts(ctx context.Context, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	bundle, err := e.capture(ctx, "HEAD", "HEAD")
	e.base = bundle.Capture.Head
	return err
}
func (e *sshNativeProbe) ArtifactLog(_ context.Context, delta string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.log += delta
	return nil
}
func (e *sshNativeProbe) FinalizeArtifacts(ctx context.Context, _ string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	bundle, err := e.capture(ctx, e.base, "HEAD")
	e.bundle = bundle
	return err
}
func (e *sshNativeProbe) NativeChange() *runner.NativeChange {
	e.mu.Lock()
	defer e.mu.Unlock()
	return &runner.NativeChange{Changed: true, ChangeID: "central-change", VersionID: "central-version", HeadSHA: e.lastDiff.HeadSHA, BaseSHA: e.lastDiff.BaseSHA}
}

func (e *sshNativeProbe) PrepareFinish(_ context.Context, _, finalMessage string, _ *tracker.NativeTerminalFailure) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finalMessage = finalMessage
	return nil
}

func (e *sshNativeProbe) ReviewVersion(context.Context) (runner.NativeValidation, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.producerFailure {
		return runner.NativeValidation{}, nil
	}
	descriptor := policy.Descriptor{Gates: policy.Gates{AutomatedReview: gate.AutomatedReviewRequired}}.WithID()
	version := tracker.ChangeVersion{ID: "central-version", ChangeID: "central-change", Policy: descriptor, ChangeVersionInput: tracker.ChangeVersionInput{BaseSHA: e.lastDiff.BaseSHA, HeadSHA: e.lastDiff.HeadSHA, Repository: e.repository, PolicyID: descriptor.ID}}
	return runner.NativeValidation{Version: &version, Diff: &connector.ValidationDiff{Repository: version.Repository, BaseSHA: version.BaseSHA, HeadSHA: version.HeadSHA, Digest: "fixture-diff", Files: []string{"feature.txt"}, Patch: e.lastDiff.Files[0].Patch}}, nil
}

func (e *sshNativeProbe) RecoverChangeSource(ctx context.Context) (workspace.ChangeSource, error) {
	input, err := e.ReviewVersion(ctx)
	return workspace.ChangeSource{Version: *input.Version}, err
}

func (*sshNativeProbe) RecordReview(context.Context, gate.ValidatorResult) error { return nil }
func (*sshNativeProbe) ProviderCapacity() *providercapacity.Reservation          { return nil }
func (*sshNativeProbe) RecordUsage(context.Context, tracker.NativeUsage) error   { return nil }
