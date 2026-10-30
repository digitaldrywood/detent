package hubclient

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

func (c *NativeClient) ChangeSource(ctx context.Context, item tracker.NativeWorkItemID, change, version string) ([]byte, error) {
	path, err := changePath(item, change, version)
	if err != nil || change == "" || version == "" {
		return nil, errors.New("valid Change Request and version identities are required")
	}
	bundle, _, err := c.client.download(ctx, c.base()+path+"/source", tracker.MaxChangeSourceBytes)
	return bundle, err
}

func (e *nativeExecution) SetChangeSource(source func(context.Context, string, string) (tracker.ChangeSourceCapture, error)) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.changeSource = source
	if source != nil {
		e.sourceRequired = true
	}
}

func (e *nativeExecution) retainChangeSource(ctx context.Context, diff tracker.AttemptDiffRequest) error {
	if !e.sourceRequired || diff.HeadSHA == diff.BaseSHA {
		return nil
	}
	if e.retainedSource != nil && e.retainedSource.Source.BaseSHA == diff.BaseSHA && e.retainedSource.Source.HeadSHA == diff.HeadSHA {
		return e.retainedSource.Source.Validate(diff.BaseSHA, diff.HeadSHA, e.retainedSource.Bundle)
	}
	if e.changeSource == nil {
		return errors.New("the finalized Change source capture is unavailable; resume on the source-owning runner before publication")
	}
	capture, err := e.changeSource(ctx, diff.BaseSHA, diff.HeadSHA)
	if err != nil {
		return err
	}
	if err := capture.Source.Validate(diff.BaseSHA, diff.HeadSHA, capture.Bundle); err != nil {
		return err
	}
	e.retainedSource = &capture
	return nil
}

func (e *nativeExecution) RecoverChangeSource(ctx context.Context) (workspace.ChangeSource, error) {
	if err := e.Validate(ctx); err != nil {
		return workspace.ChangeSource{}, err
	}
	detail, err := e.claim.source.client.currentChange(ctx, e.claim.lease.WorkItemID)
	if err != nil {
		return workspace.ChangeSource{}, err
	}
	recovery := e.Recovery()
	selected, selectedChange := recovery.SourceVersionID, detail.Change.ID
	e.mu.Lock()
	if e.change != nil && e.change.VersionID != "" {
		selected, selectedChange = e.change.VersionID, e.change.ChangeID
	} else if e.recoveredSource != nil {
		selected, selectedChange = e.recoveredSource.VersionID, e.recoveredSource.ChangeID
	} else if recovery.Change != nil && recovery.Change.VersionID == selected {
		selectedChange = recovery.Change.ChangeID
	}
	canAdvance := e.change == nil && e.retainedSource == nil && publishedChangeSourceSupersedes(detail, selected, recovery)
	e.mu.Unlock()
	if selected != "" && (selectedChange != detail.Change.ID || selected != detail.Change.CurrentVersion && !canAdvance) {
		return workspace.ChangeSource{}, fmt.Errorf("%w (Change %q, selected version %q, current version %q)", runner.ErrChangeSourceVersionSuperseded, detail.Change.ID, selected, detail.Change.CurrentVersion)
	}
	if detail.Change.CurrentVersion == "" {
		return workspace.ChangeSource{}, nil
	}
	for _, version := range detail.Versions {
		if version.ID != detail.Change.CurrentVersion {
			continue
		}
		source := workspace.ChangeSource{Version: version}
		source.Transferred = version.ID == e.Recovery().SourceVersionID
		for _, previous := range detail.Versions {
			if previous.Number <= 0 || previous.Number >= version.Number || previous.Repository != version.Repository || previous.Source == nil ||
				previous.Source.Format != "git-bundle" || previous.Source.HeadSHA != previous.HeadSHA || previous.Source.BaseSHA != previous.BaseSHA ||
				previous.Source.Bytes <= 0 || previous.Source.BundleSHA256 == "" || previous.Source.DiffSHA256 == "" {
				continue
			}
			source.PreviousHeads = append(source.PreviousHeads, previous.HeadSHA)
		}
		if version.Source != nil {
			source.Bundle, err = e.claim.source.client.ChangeSource(ctx, e.claim.lease.WorkItemID, detail.Change.ID, version.ID)
			if err == nil {
				err = version.Source.Validate(version.BaseSHA, version.HeadSHA, source.Bundle)
			}
			if err != nil {
				var apiErr *APIError
				if errors.As(err, &apiErr) && apiErr.Status < 500 {
					return workspace.ChangeSource{}, err
				}
				capture, recoveryErr := e.recoverLocalChangeSource(ctx, version)
				if recoveryErr != nil {
					return workspace.ChangeSource{}, errors.Join(err, runner.ErrNativeRecoveryRequired, recoveryErr)
				}
				source.Bundle, source.Version.Source = capture.Bundle, &capture.Source
			}
		}
		if err := e.Validate(ctx); err != nil {
			return workspace.ChangeSource{}, err
		}
		e.mu.Lock()
		e.recoveredSource = &tracker.NativeChangeReference{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA}
		e.mu.Unlock()
		return source, nil
	}
	return workspace.ChangeSource{}, runner.ErrNativeRecoveryRequired
}

func publishedChangeSourceSupersedes(detail tracker.ChangeDetail, selected string, recovery tracker.NativeRecovery) bool {
	var previous, current *tracker.ChangeVersion
	for i := range detail.Versions {
		version := &detail.Versions[i]
		if version.ID == selected {
			previous = version
		}
		if version.ID == detail.Change.CurrentVersion {
			current = version
		}
	}
	if previous == nil || current == nil || previous.ChangeID != detail.Change.ID || current.ChangeID != detail.Change.ID ||
		previous.Number <= 0 || previous.Number >= current.Number || previous.Repository != current.Repository || previous.Source == nil || current.Source == nil {
		return false
	}
	if owner := recovery.SourceAttempt(); owner != nil && owner.Checkpoint.PreservesSource() {
		checkpoint := owner.Checkpoint
		if checkpoint.UncertainForgeEffect() || checkpoint.HeadSHA != previous.HeadSHA && checkpoint.HeadSHA != current.HeadSHA || checkpoint.WorktreeState != "clean" && checkpoint.WorktreeState != "unpushed" {
			return false
		}
	}
	return true
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
