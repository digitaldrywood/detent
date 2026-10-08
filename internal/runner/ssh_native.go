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
	landingBatch        func(context.Context, []workspace.LandRequest) []sshLandingOutcome
	mu                  sync.Mutex
	diff                AttemptDiffSource
	directory           string
	evidence            func(context.Context, string) (ValidationEvidence, error)
	integration         func(context.Context, tracker.ChangeVersion, string) (workspace.LandResult, error)
	changeSource        func(context.Context, string, string) (tracker.ChangeSourceCapture, error)
	publication         func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)
	publicationIdentity func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)
	publicationPeer     *SSHPeer
}

type sshDiff struct {
	Request tracker.AttemptDiffRequest
	Present bool
}

func (s *SSHExecutionSources) Handle(ctx context.Context, method string, args []json.RawMessage) (any, error) {
	s.mu.Lock()
	diff, directory, evidence, integration, changeSource := s.diff, s.directory, s.evidence, s.integration, s.changeSource
	publication, publicationIdentity, publicationPeer := s.publication, s.publicationIdentity, s.publicationPeer
	landingBatch := s.landingBatch
	s.mu.Unlock()
	switch method {
	case "source.publication":
		return invokeSSHMethod(ctx, func(ctx context.Context, invocation uint64, version tracker.ChangeVersion, opts workspace.LandOptions, identity *workspace.GitHubPublication) (workspace.GitHubPublication, error) {
			if identity == nil {
				if invocation != 0 || publicationIdentity == nil {
					return workspace.GitHubPublication{}, errors.New("SSH publication identity is unavailable")
				}
				return publicationIdentity(ctx, version, opts)
			}
			if invocation == 0 || publication == nil || publicationPeer == nil || identity.Repository != version.Repository || identity.HeadSHA != version.HeadSHA || identity.Branch == "" || identity.BaseRef == "" {
				return workspace.GitHubPublication{}, errors.New("SSH publication invocation is unavailable or unbound")
			}
			opts.Repository, opts.HeadSHA, opts.TargetBranch = identity.Repository, identity.HeadSHA, identity.BaseRef
			opts.Authorize = func(ctx context.Context) error {
				return publicationPeer.Call(ctx, "publication.authorize", nil, invocation, version.ID, *identity)
			}
			opts.PublicationEffect = func(ctx context.Context, kind, state string, result workspace.GitHubPublication) error {
				return publicationPeer.Call(ctx, "publication.effect", nil, invocation, version.ID, *identity, kind, state, result)
			}
			return publication(ctx, version, opts)
		}, "", args)
	case "source.landing_batch":
		if landingBatch == nil {
			return nil, errors.New("SSH landing batch source is unavailable")
		}
		return invokeSSHMethod(ctx, landingBatch, "", args)
	case "source.change":
		if changeSource == nil {
			if directory == "" {
				return nil, errors.New("SSH Change source workspace is unavailable")
			}
			changeSource = func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
				return workspace.CaptureChangeSource(ctx, directory, base, head)
			}
		}
		return invokeSSHMethod(ctx, changeSource, "", args)
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
	c.mu.Lock()
	c.batchLander = &sshBatchLander{peer: peer, callbacks: c}
	c.mu.Unlock()
	if source, ok := c.execution.(ChangeSourceExecution); ok {
		source.SetChangeSource(func(ctx context.Context, base, head string) (tracker.ChangeSourceCapture, error) {
			var result tracker.ChangeSourceCapture
			err := peer.Call(ctx, "source.change", &result, base, head)
			return result, err
		})
	}
	if source, ok := c.execution.(PublicationSourceExecution); ok {
		source.SetPublicationSource(func(ctx context.Context, version tracker.ChangeVersion, opts workspace.LandOptions) (workspace.GitHubPublication, error) {
			c.publicationMu.Lock()
			defer c.publicationMu.Unlock()
			if opts.Authorize == nil || opts.PublicationEffect == nil {
				return workspace.GitHubPublication{}, errors.New("SSH publication requires private fenced authority and checkpoint callbacks")
			}
			if err := opts.Authorize(ctx); err != nil {
				return workspace.GitHubPublication{}, err
			}
			wire := opts
			wire.Authorize, wire.PublicationEffect, wire.GitHubClient = nil, nil, nil
			var identity workspace.GitHubPublication
			if err := peer.Call(ctx, "source.publication", &identity, uint64(0), version, wire, nil); err != nil {
				return workspace.GitHubPublication{}, err
			}
			if identity.Repository != version.Repository || identity.HeadSHA != version.HeadSHA || identity.Branch == "" || identity.BaseRef == "" || identity.External.ID != "" || identity.External.URL != "" || opts.TargetBranch != "" && opts.TargetBranch != identity.BaseRef {
				return workspace.GitHubPublication{}, errors.New("SSH publication target differs from the current version and configured base")
			}
			if err := opts.Authorize(ctx); err != nil {
				return workspace.GitHubPublication{}, err
			}
			c.mu.Lock()
			if c.closed {
				c.mu.Unlock()
				return workspace.GitHubPublication{}, errors.New("SSH publication channel is closed")
			}
			c.publicationSequence++
			invocation := c.publicationSequence
			c.publicationAuthorize = func(id uint64, versionID string, target workspace.GitHubPublication) error {
				if id != invocation || versionID != version.ID || target != identity {
					return errors.New("SSH publication callback differs from the active invocation")
				}
				if err := context.Cause(ctx); err != nil {
					return err
				}
				return opts.Authorize(ctx)
			}
			c.publicationEffect = func(id uint64, versionID string, target workspace.GitHubPublication, kind, state string, result workspace.GitHubPublication) error {
				if err := c.publicationAuthorize(id, versionID, target); err != nil {
					return err
				}
				actual := result
				actual.External = tracker.ChangeExternalReference{}
				if actual != identity || kind != "git_push" && kind != "pr_create" || state != "pending" && state != "confirmed" && state != "ambiguous" && state != "none" {
					return errors.New("SSH publication effect differs from its bound source or checkpoint operation")
				}
				return opts.PublicationEffect(ctx, kind, state, result)
			}
			c.mu.Unlock()
			defer func() { c.mu.Lock(); c.publicationAuthorize, c.publicationEffect = nil, nil; c.mu.Unlock() }()
			var result workspace.GitHubPublication
			if err := peer.Call(ctx, "source.publication", &result, invocation, version, wire, &identity); err != nil {
				return workspace.GitHubPublication{}, err
			}
			actual := result
			actual.External = tracker.ChangeExternalReference{}
			if actual != identity {
				return workspace.GitHubPublication{}, errors.New("SSH publication returned a different source identity")
			}
			if err := opts.Authorize(ctx); err != nil {
				return workspace.GitHubPublication{}, err
			}
			return result, nil
		})
	}
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
	batchedLanding bool
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

func (e *sshNativeExecution) SetPublicationSource(source func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)) {
	e.sources.mu.Lock()
	defer e.sources.mu.Unlock()
	e.sources.publication, e.sources.publicationPeer = source, e.peer
}

func (e *sshNativeExecution) setPublicationIdentity(source func(context.Context, tracker.ChangeVersion, workspace.LandOptions) (workspace.GitHubPublication, error)) {
	e.sources.mu.Lock()
	defer e.sources.mu.Unlock()
	e.sources.publicationIdentity = source
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

func (e *sshNativeExecution) SetChangeSource(source func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
	e.sources.mu.Lock()
	defer e.sources.mu.Unlock()
	e.sources.changeSource = source
}

func (e *sshNativeExecution) RecoverChangeSource(ctx context.Context) (workspace.ChangeSource, error) {
	var result workspace.ChangeSource
	err := e.peer.Call(ctx, "execution.RecoverChangeSource", &result)
	return result, err
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

func (e *sshNativeExecution) RecordSourceValidation(ctx context.Context, result gate.CommandResult) error {
	return e.peer.Call(ctx, "execution.RecordSourceValidation", nil, result)
}
