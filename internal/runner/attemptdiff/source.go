package attemptdiff

import (
	"context"
	"log/slog"
	"strings"

	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/workspace"
)

type Source = func(context.Context) (tracker.AttemptDiffRequest, bool)

type verificationKey struct{}

func Verify(ctx context.Context) context.Context {
	return context.WithValue(ctx, verificationKey{}, true)
}

func VerificationRequested(ctx context.Context) bool { return ctx.Value(verificationKey{}) == true }

func New(ctx context.Context, logger *slog.Logger, info workspace.Info, issue workspace.Issue) Source {
	base := strings.TrimSpace(issue.BaseRef)
	if base == "" {
		base = workspace.AttemptBase(ctx, info.Path)
	}
	return func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
		diffs, err := workspace.GitFileDiffs(ctx, info.Path, base, tracker.MaxDiffBytes)
		if err != nil {
			logger.Warn("attempt diff unavailable", "issue_id", issue.ID, "workspace_path", info.Path, "error", err)
			return tracker.AttemptDiffRequest{}, false
		}
		request := tracker.AttemptDiffRequest{BaseSHA: diffs.BaseSHA, HeadSHA: diffs.HeadSHA, Files: make([]tracker.AttemptDiffFile, 0, len(diffs.Files))}
		for _, file := range diffs.Files {
			request.Files = append(request.Files, tracker.AttemptDiffFile{
				Path: file.Path, OldPath: file.OldPath, Status: file.Status,
				Additions: file.Additions, Deletions: file.Deletions, Binary: file.Binary, Patch: file.Patch,
			})
		}
		if diffs.Truncated {
			// The whole patch output exceeded the bound, so the counts are
			// posted without patches rather than with a patch set that stops
			// partway through the change.
			request.Files = tracker.StripDiffPatches(request.Files)
		}
		return request, true
	}
}

func BindValidation(ctx context.Context, source Source, gate tracker.NativeCheckpoint, state func(context.Context) *workspace.RecoveryState) Source {
	original, captured := source(ctx)
	return func(ctx context.Context) (tracker.AttemptDiffRequest, bool) {
		current, ok := source(ctx)
		current.WorkspaceDigest = ""
		if VerificationRequested(ctx) && ok && captured && original.BaseSHA != "" && original.HeadSHA == gate.HeadSHA && original.SameContent(current) {
			if live := state(ctx); live != nil && live.HeadSHA == gate.HeadSHA && live.WorkspaceFingerprint == gate.WorkspaceDigest {
				current.WorkspaceDigest = live.WorkspaceFingerprint
			}
		}
		return current, ok
	}
}
