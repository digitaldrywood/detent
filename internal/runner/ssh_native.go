package runner

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

// SSHExecutionSources exposes only the checkout bound by the remote runner.
// Upload credentials, journals, producer tuples and lease ownership stay central.
type SSHExecutionSources struct {
	mu          sync.Mutex
	diff        AttemptDiffSource
	directory   string
	evidence    func(context.Context, string) (ValidationEvidence, error)
	integration func(context.Context, tracker.ChangeVersion, string) (workspace.LandResult, error)
}

type sshDiff struct {
	Request tracker.AttemptDiffRequest
	Present bool
}

func (s *SSHExecutionSources) Handle(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	s.mu.Lock()
	diff, directory, evidence, integration := s.diff, s.directory, s.evidence, s.integration
	s.mu.Unlock()
	switch method {
	case "source.integration":
		if integration == nil {
			return nil, errors.New("SSH integration workspace is unavailable")
		}
		return invokeSSHMethod(ctx, integration, "", args)
	case "source.evidence":
		if evidence == nil {
			return nil, errors.New("SSH evidence workspace is unavailable")
		}
		return invokeSSHMethod(ctx, evidence, "", args)
	case "source.diff":
		if len(args) != 0 {
			return nil, errors.New("invalid SSH diff arguments")
		}
		if diff == nil {
			return sshDiff{}, nil
		}
		request, present := diff(ctx)
		return sshDiff{Request: request, Present: present}, nil
	case "source.capture":
		if directory == "" {
			return nil, errors.New("SSH artifact workspace is unavailable")
		}
		return invokeSSHMethod(ctx, func(ctx context.Context, base, head string) (artifact.GitCapture, error) {
			return artifact.CaptureGit(ctx, directory, base, head, 3)
		}, "", args)
	default:
		return nil, errors.New("unsupported SSH source method")
	}
}

// BindExecutionSources installs host-aware sources on the central execution.
// The root comes from central workspace configuration; a remote path is never
// interpreted as a local path. Finish can use the last checkpoint diff once the
// channel closes, just as it does after a local checkout is removed.
func (c *SSHCallbacks) BindExecutionSources(peer *SSHPeer, journalRoot string) {
	if source, ok := c.execution.(IntegrationSourceExecution); ok {
		source.SetIntegrationSource(func(ctx context.Context, version tracker.ChangeVersion, base string) (workspace.LandResult, error) {
			var result workspace.LandResult
			err := peer.Call(ctx, "source.integration", &result, version, base)
			return result, err
		})
	}
	if source, ok := c.execution.(EvidenceSourceExecution); ok {
		source.SetEvidenceSource(func(ctx context.Context, path string) (ValidationEvidence, error) {
			var result ValidationEvidence
			err := peer.Call(ctx, "source.evidence", &result, path)
			return result, err
		})
	}
	if source, ok := c.execution.(ArtifactSourceExecution); ok {
		source.SetArtifactSource(journalRoot, func(ctx context.Context, base, head string) (artifact.GitCapture, error) {
			var result artifact.GitCapture
			err := peer.Call(ctx, "source.capture", &result, base, head)
			return result, err
		})
	}
	if diff, ok := c.execution.(DiffExecution); ok {
		diff.SetDiffSource(func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
			var result sshDiff
			if err := peer.Call(ctx, "source.diff", &result); err != nil {
				return tracker.AttemptDiffRequest{}, false
			}
			return result.Request, result.Present
		})
	}
}

type sshNativeExecution struct {
	*sshExecution
	sources  *SSHExecutionSources
	capacity *providercapacity.Reservation
	mu       sync.Mutex
	setupErr error
}

func (e *sshNativeExecution) SetEvidenceSource(source func(context.Context, string) (ValidationEvidence, error)) {
	e.sources.mu.Lock()
	defer e.sources.mu.Unlock()
	e.sources.evidence = source
}

func (e *sshNativeExecution) SetDiffSource(source AttemptDiffSource) {
	e.sources.mu.Lock()
	defer e.sources.mu.Unlock()
	e.sources.diff = source
}

func (e *sshNativeExecution) SetIntegrationSource(source func(context.Context, tracker.ChangeVersion, string) (workspace.LandResult, error)) {
	e.sources.mu.Lock()
	defer e.sources.mu.Unlock()
	e.sources.integration = source
}

func (e *sshNativeExecution) SetRepository(repository string) {
	err := e.peer.Call(e.peer.Context(), "execution.SetRepository", nil, repository)
	e.mu.Lock()
	defer e.mu.Unlock()
	e.setupErr = errors.Join(e.setupErr, err)
}

func (e *sshNativeExecution) Start(ctx context.Context, identity tracker.NativeExecutionIdentity) error {
	e.mu.Lock()
	err := e.setupErr
	e.mu.Unlock()
	if err != nil {
		return err
	}
	return e.sshExecution.Start(ctx, identity)
}

func (e *sshNativeExecution) PrepareArtifacts(ctx context.Context, directory string) error {
	e.sources.mu.Lock()
	e.sources.directory = directory
	e.sources.mu.Unlock()
	return e.peer.Call(ctx, "execution.PrepareArtifacts", nil, directory)
}

func (e *sshNativeExecution) ArtifactLog(ctx context.Context, delta string) error {
	return e.peer.Call(ctx, "execution.ArtifactLog", nil, delta)
}

func (e *sshNativeExecution) FinalizeArtifacts(ctx context.Context, directory string) error {
	return e.peer.Call(ctx, "execution.FinalizeArtifacts", nil, directory)
}

func (e *sshNativeExecution) LandingTarget(ctx context.Context) (NativeLandingTarget, error) {
	var result NativeLandingTarget
	err := e.peer.Call(ctx, "execution.LandingTarget", &result)
	return result, err
}

func (e *sshNativeExecution) RecordLanding(ctx context.Context, landing NativeLanding) error {
	return e.peer.Call(ctx, "execution.RecordLanding", nil, landing)
}

func (e *sshNativeExecution) ProviderCapacity() *providercapacity.Reservation { return e.capacity }

func (e *sshNativeExecution) AvailabilityDeadline() time.Time {
	var result time.Time
	if err := e.peer.Call(e.peer.Context(), "execution.AvailabilityDeadline", &result); err != nil {
		return time.Time{}
	}
	return result
}

func (e *sshNativeExecution) RecordUsage(ctx context.Context, usage tracker.NativeUsage) error {
	return e.peer.Call(ctx, "execution.RecordUsage", nil, usage)
}

func (e *sshNativeExecution) ObserveRuntime(ctx context.Context, observation tracker.NativeRuntimeObservation) error {
	return e.peer.Call(ctx, "execution.ObserveRuntime", nil, observation)
}
func (e *sshNativeExecution) StartLanding(ctx context.Context, attempt int64, generation uint64) error {
	return e.peer.Call(ctx, "execution.StartLanding", nil, attempt, generation)
}
func (e *sshNativeExecution) ObserveLanding(ctx context.Context, landing NativeLanding) error {
	return e.peer.Call(ctx, "execution.ObserveLanding", nil, landing)
}

func (e *sshNativeExecution) PublishValidationEvidence(ctx context.Context, files []ValidationEvidence) error {
	return e.peer.Call(ctx, "execution.PublishValidationEvidence", nil, files)
}

func (e *sshNativeExecution) PrepareFinish(ctx context.Context, outcome, finalMessage string, failure *tracker.NativeTerminalFailure) error {
	return e.peer.Call(ctx, "execution.PrepareFinish", nil, outcome, finalMessage, failure)
}

func (e *sshNativeExecution) ValidatorVersion(ctx context.Context) (NativeValidation, error) {
	var input NativeValidation
	err := e.peer.Call(ctx, "execution.ValidatorVersion", &input)
	return input, err
}

func (e *sshNativeExecution) RecordValidator(ctx context.Context, result gate.ValidatorResult) error {
	return e.peer.Call(ctx, "execution.RecordValidator", nil, result)
}
