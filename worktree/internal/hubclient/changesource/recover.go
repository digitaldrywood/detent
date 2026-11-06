package changesource

import (
	"context"
	"errors"
	"fmt"

	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type Reader interface {
	CurrentChange(context.Context, tracker.NativeWorkItemID) (tracker.ChangeDetail, error)
	ChangeSource(context.Context, tracker.NativeWorkItemID, string, string) ([]byte, error)
}
type Authority interface {
	Recovery() tracker.NativeRecovery
	Selection(tracker.ChangeDetail, tracker.NativeRecovery) (string, string, bool)
	Recapture(context.Context, tracker.ChangeVersion) (tracker.ChangeSourceCapture, error)
	Validate(context.Context) error
	Accept(tracker.NativeChangeReference)
}

func Recover(ctx context.Context, reader Reader, item tracker.NativeWorkItemID, host Authority) (workspace.ChangeSource, error) {
	detail, err := reader.CurrentChange(ctx, item)
	if err != nil {
		return workspace.ChangeSource{}, err
	}
	recovery := host.Recovery()
	selected, selectedChange, canAdvance := host.Selection(detail, recovery)
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
		source.Transferred = version.ID == host.Recovery().SourceVersionID
		for _, previous := range detail.Versions {
			if previous.Number <= 0 || previous.Number >= version.Number || previous.Repository != version.Repository || previous.Source == nil ||
				previous.Source.Format != "git-bundle" || previous.Source.HeadSHA != previous.HeadSHA || previous.Source.BaseSHA != previous.BaseSHA ||
				previous.Source.Bytes <= 0 || previous.Source.BundleSHA256 == "" || previous.Source.DiffSHA256 == "" {
				continue
			}
			source.PreviousHeads = append(source.PreviousHeads, previous.HeadSHA)
		}
		if version.Source != nil {
			source.Bundle, err = reader.ChangeSource(ctx, item, detail.Change.ID, version.ID)
			if err == nil {
				err = version.Source.Validate(version.BaseSHA, version.HeadSHA, source.Bundle)
			}
			if err != nil {
				var apiErr *api.APIError
				if errors.As(err, &apiErr) && apiErr.Status < 500 {
					return workspace.ChangeSource{}, err
				}
				capture, recoveryErr := host.Recapture(ctx, version)
				if recoveryErr != nil {
					return workspace.ChangeSource{}, errors.Join(err, runner.ErrNativeRecoveryRequired, recoveryErr)
				}
				source.Bundle, source.Version.Source = capture.Bundle, &capture.Source
			}
		}
		if err := host.Validate(ctx); err != nil {
			return workspace.ChangeSource{}, err
		}
		host.Accept(tracker.NativeChangeReference{ChangeID: detail.Change.ID, VersionID: version.ID, HeadSHA: version.HeadSHA})
		return source, nil
	}
	return workspace.ChangeSource{}, runner.ErrNativeRecoveryRequired
}
