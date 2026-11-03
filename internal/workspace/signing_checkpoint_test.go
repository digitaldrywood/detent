package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/testenv"
)

type signingDeadlineContext struct{ context.Context }

func (c signingDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.Cause(c.Context)
	}
	return nil
}

func TestSigningDeadlinePreservesCompletedSource(t *testing.T) {
	for _, stage := range []string{"before_turn", "finalization", "startup_unavailable"} {
		t.Run(stage, func(t *testing.T) {
			source := initSourceRepo(t)
			remote := initBareRemote(t)
			runGit(t, source, "remote", "add", "origin", remote)
			runGit(t, source, "push", "-u", "origin", "main")
			owner, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{ID: "signing-deadline", Identifier: "example#1", BaseRef: "main", ProgressBaseRef: "main"}
			info, err := owner.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			writeCheckpointFile(t, info.Path, "completed.md", "completed source\n")
			runGit(t, info.Path, "add", "completed.md")
			head := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			tree := strings.TrimSpace(runGit(t, info.Path, "write-tree"))
			ready := filepath.Join(t.TempDir(), "ready")
			signer := filepath.Join(t.TempDir(), "signer")
			if err := os.WriteFile(signer, []byte("#!/bin/sh\nif [ -f "+shellQuote(ready)+" ]; then exit 1; fi\nprintf ready > "+shellQuote(ready)+"\nexec sleep 600\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			owner.commits.program, owner.commits.format = signer, "openpgp"
			owner.commits.report.Signing = "openpgp"
			owner.commits.signingRequested = true
			if stage == "startup_unavailable" {
				owner.commits.report.Signing = "unsigned"
				if err := owner.BeforeRun(t.Context(), info, issue); err == nil {
					t.Fatal("startup signer failure allowed a turn")
				}
				return
			}
			ctx, expire := context.WithCancelCause(t.Context())
			defer expire(context.Canceled)
			done := make(chan error, 1)
			go func() {
				defer close(done)
				ctx := signingDeadlineContext{ctx}
				if stage == "before_turn" {
					done <- owner.BeforeRun(ctx, info, issue)
					return
				}
				_, err := owner.FinalizeNativeWork(ctx, info, issue, func(ctx context.Context) error { return ctx.Err() })
				done <- err
			}()
			defer func() {
				expire(context.Canceled)
				for range done {
				}
			}()
			testenv.WaitUntil(t, t.Context(), func() error { _, err := os.Stat(ready); return err })
			expire(context.DeadlineExceeded)
			if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("signer deadline = %v", err)
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD")); got != head {
				t.Fatal("signer failure advanced HEAD")
			}
			if got := strings.TrimSpace(runGit(t, info.Path, "write-tree")); got != tree {
				t.Fatal("signer failure changed staged source")
			}
			ref, pushed, err := owner.PushCheckpointRef(t.Context(), info, issue, CheckpointRef{}, func(ctx context.Context) error { return ctx.Err() })
			if err != nil || !pushed || ref.TreeSHA != tree {
				t.Fatalf("completed checkpoint: %+v, pushed=%v, error=%v", ref, pushed, err)
			}
			if got := runGit(t, remote, "show", ref.Ref+":completed.md"); got != "completed source\n" {
				t.Fatalf("remote lost completed source: %q", got)
			}
			if owner.CommitReport().Signing != "openpgp" {
				t.Fatal("deadline downgraded commit signing")
			}
		})
	}
}
