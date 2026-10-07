package hubclient

import (
	"context"
	"errors"

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
	if !e.sourceRequired || diff.HeadSHA == diff.BaseSHA || len(diff.Files) == 0 {
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
	if detail.Change.CurrentVersion == "" {
		return workspace.ChangeSource{}, nil
	}
	for _, version := range detail.Versions {
		if version.ID != detail.Change.CurrentVersion {
			continue
		}
		source := workspace.ChangeSource{Version: version}
		if version.Source != nil {
			source.Bundle, err = e.claim.source.client.ChangeSource(ctx, e.claim.lease.WorkItemID, detail.Change.ID, version.ID)
			if err != nil {
				return workspace.ChangeSource{}, err
			}
			if err := version.Source.Validate(version.BaseSHA, version.HeadSHA, source.Bundle); err != nil {
				return workspace.ChangeSource{}, err
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
