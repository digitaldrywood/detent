package nativechange

import (
	"context"
	"errors"

	"github.com/digitaldrywood/detent/internal/hubclient/changesource"
	"github.com/digitaldrywood/detent/internal/hubclient/checkpoint"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (e *nativeExecution) SetChangeSource(source func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.changeSource = source
	if source != nil {
		e.sourceRequired = true
	}
}

func (e *nativeExecution) retainChangeSource(ctx context.Context, diff tracker.AttemptDiffRequest) error {
	result, err := changesource.Retain(ctx, e.sourceRequired, e.retainedSource, diff, e.changeSource)
	if err == nil {
		e.retainedSource = result.Capture
	}
	return err
}

func (e *nativeExecution) RecoverChangeSource(ctx context.Context) (workspace.ChangeSource, error) {
	if err := e.Validate(ctx); err != nil {
		return workspace.ChangeSource{}, err
	}
	e.mu.Lock()
	if e.change != nil && e.change.NoChange != nil {
		version := e.noChangeVersion()
		e.mu.Unlock()
		return workspace.ChangeSource{Version: version}, nil
	}
	e.mu.Unlock()
	return changesource.Recover(ctx, e.claim.source.NativeClient(), e.claim.lease.WorkItemID, sourceAuthority{e})
}

func publishedChangeSourceSupersedes(detail tracker.ChangeDetail, selected string, recovery tracker.NativeRecovery) bool {
	return checkpoint.SourceSupersedes(detail, selected, recovery)
}

func (e *nativeExecution) requireRecoveredSource(detail tracker.ChangeDetail, diff tracker.AttemptDiffRequest) error {
	if e.recoveredSource == nil || detail.Change.ID == e.recoveredSource.ChangeID && detail.Change.CurrentVersion == e.recoveredSource.VersionID {
		return nil
	}
	for _, version := range detail.Versions {
		if detail.Change.ID == e.recoveredSource.ChangeID && version.ID == detail.Change.CurrentVersion && version.AttemptID == e.data.AttemptID && version.RunID == e.data.RunID &&
			version.HeadSHA == diff.HeadSHA && version.BaseSHA == diff.BaseSHA && version.PolicyID == e.data.PolicyID {
			return nil
		}
	}
	return errors.New("the current Change version differs from the recovered source; reconcile the newer version before publishing")
}

func (e *nativeExecution) recoverLocalChangeSource(ctx context.Context, version tracker.ChangeVersion) (tracker.ChangeSourceCapture, error) {
	recovery := e.Recovery()
	owner := recovery.SourceAttempt()
	if owner == nil || owner.Checkpoint == nil || owner.MachineID != recovery.Lease.MachineID || owner.Checkpoint.HeadSHA != version.HeadSHA || owner.Checkpoint.Availability != "available" || owner.Checkpoint.Storage != "local_only" && !owner.Checkpoint.GitRef() || e.changeSource == nil {
		return tracker.ChangeSourceCapture{}, errors.New("retained source is unavailable; verify the exact local checkpoint on its source runner before recapture")
	}
	capture, err := e.changeSource(ctx, version.BaseSHA, version.HeadSHA)
	if err != nil {
		return capture, err
	}
	if err := capture.Source.Validate(version.BaseSHA, version.HeadSHA, capture.Bundle); err != nil {
		return capture, err
	}
	if version.Source != nil && capture.Source.DiffSHA256 != version.Source.DiffSHA256 {
		return capture, errors.New("local source differs from the immutable Change version")
	}
	if err := e.Validate(ctx); err != nil {
		return capture, err
	}
	return capture, nil
}

func (e *nativeExecution) verifiedEmptyRework(diff tracker.AttemptDiffRequest) bool {
	return e.role == runner.RoleRework && e.recoveredSource != nil && e.sourceRequired &&
		diff.BaseSHA != diff.HeadSHA && len(diff.Files) == 0 && e.retainedSource != nil &&
		e.retainedSource.Source.Validate(diff.BaseSHA, diff.HeadSHA, e.retainedSource.Bundle) == nil &&
		e.retainedSource.Source.DiffSHA256 == tracker.ChangeSourceDigest(nil)
}

type sourceAuthority struct{ *nativeExecution }

func (a sourceAuthority) Selection(detail tracker.ChangeDetail, recovery tracker.NativeRecovery) (string, string, bool) {
	e := a.nativeExecution
	e.mu.Lock()
	defer e.mu.Unlock()
	selected, selectedChange := recovery.SourceVersionID, detail.Change.ID
	if e.change != nil && e.change.VersionID != "" {
		selected, selectedChange = e.change.VersionID, e.change.ChangeID
	} else if e.recoveredSource != nil {
		selected, selectedChange = e.recoveredSource.VersionID, e.recoveredSource.ChangeID
	} else if recovery.Change != nil && recovery.Change.VersionID == selected {
		selectedChange = recovery.Change.ChangeID
	}
	canAdvance := e.change == nil && e.retainedSource == nil && publishedChangeSourceSupersedes(detail, selected, recovery)

	return selected, selectedChange, canAdvance
}
func (a sourceAuthority) Recapture(ctx context.Context, version tracker.ChangeVersion) (tracker.ChangeSourceCapture, error) {
	return a.recoverLocalChangeSource(ctx, version)
}
func (a sourceAuthority) Accept(source tracker.NativeChangeReference) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.recoveredSource = &source
}
