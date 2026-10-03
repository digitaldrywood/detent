package cli

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
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
func (e *sshNativeProbe) Finish(context.Context, string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.finished++
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
func (*sshNativeProbe) SetRepository(string) {}
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
func (*sshNativeProbe) NativeChange() *runner.NativeChange {
	return &runner.NativeChange{Changed: true, ChangeID: "central-change", VersionID: "central-version"}
}
func (*sshNativeProbe) ProviderCapacity() *providercapacity.Reservation        { return nil }
func (*sshNativeProbe) RecordUsage(context.Context, tracker.NativeUsage) error { return nil }
