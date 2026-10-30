package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"testing/synctest"
)

func TestWorkspaceUseAndCleanup(t *testing.T) {
	for _, workerFirst := range []bool{false, true} {
		name := "cleanup first"
		if workerFirst {
			name = "worker first"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var uses workspaceUses
				if workerFirst {
					release, err := uses.use(t.Context(), "same")
					if err != nil {
						t.Fatal(err)
					}
					if _, ok := uses.cleanup("same"); ok {
						t.Fatal("cleanup acquired an active workspace")
					}
					other, ok := uses.cleanup("other")
					if !ok {
						t.Fatal("unrelated workspace was blocked")
					}
					other()
					release()
					cleanup, ok := uses.cleanup("same")
					if !ok {
						t.Fatal("completed workspace remains in use")
					}
					cleanup()
				} else {
					release, ok := uses.cleanup("same")
					if !ok {
						t.Fatal("cleanup acquisition failed")
					}
					started := make(chan struct{})
					go func() {
						done, err := uses.use(t.Context(), "same")
						if err != nil {
							t.Error(err)
							return
						}
						close(started)
						done()
					}()
					synctest.Wait()
					select {
					case <-started:
						t.Fatal("worker entered workspace during deletion")
					default:
					}
					release()
					synctest.Wait()
					select {
					case <-started:
					default:
						t.Fatal("worker did not resume after deletion")
					}
				}
			})
		})
	}
}

func TestWorkspaceUseCancellation(t *testing.T) {
	var uses workspaceUses
	release, _ := uses.cleanup("path")
	defer release()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := uses.use(ctx, "path"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestFilesystemCleanupProtectsWorker(t *testing.T) {
	for _, kind := range []string{KindFilesystem, KindLocalGit} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
			t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
			opts := LocalGitOptions{Root: t.TempDir(), SourceRoot: t.TempDir(), CommitIdentity: CommitIdentity{Name: "Enrolled runner", Email: "runner@example.com"}}
			backend, err := NewBackend(kind, opts)
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "owner/repository#1", ID: "1"}
			release, err := backend.(Usage).Use(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			if kind == KindLocalGit {
				if err := os.MkdirAll(filepath.Join(opts.Root, "owner_repository_1"), 0700); err != nil {
					t.Fatal(err)
				}
			} else if _, err := backend.Create(t.Context(), issue); err != nil {
				t.Fatal(err)
			}
			opts.Hooks.AfterCreate = "printf updated"
			opts.CommitIdentity = CommitIdentity{Name: "Changed identity", Email: "changed@example.com"}
			updated, err := ReconfigureBackend(backend, kind, opts)
			if err != nil {
				t.Fatal(err)
			}
			if kind == KindLocalGit {
				before := backend.(*LocalGit).CommitReport()
				after := updated.(*LocalGit).CommitReport()
				if after != before || after.Identity.Email != "runner@example.com" {
					t.Fatalf("reload changed startup commit policy: before=%+v after=%+v", before, after)
				}
			}
			for _, candidate := range []Backend{backend, updated} {
				if _, err := candidate.(IssueCleaner).CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
					t.Fatalf("cleanup=%v", err)
				}
			}
			if _, err := os.Stat(filepath.Join(opts.Root, "owner_repository_1")); err != nil {
				t.Fatalf("active workspace removed: %v", err)
			}
		})
	}
}

func TestFilesystemCleanupBatch(t *testing.T) {
	for _, expired := range []bool{false, true} {
		for _, count := range []int{0, 1, 10, 11, 50} {
			t.Run(fmt.Sprintf("expired=%v/count=%d", expired, count), func(t *testing.T) {
				backend, err := NewFilesystem(FilesystemOptions{Root: t.TempDir()})
				if err != nil {
					t.Fatal(err)
				}
				ctx := WithCleanupBatch(t.Context(), 10)
				admission, cancel := context.WithCancel(t.Context())
				defer cancel()
				ctx = WithCleanupAdmission(ctx, admission)
				if expired {
					cancel()
				}
				for i := range count {
					issue := Issue{Identifier: fmt.Sprintf("repo#%d", i), ID: strconv.Itoa(i)}
					info, err := backend.Create(t.Context(), issue)
					if err != nil {
						t.Fatal(err)
					}
					_, err = backend.CleanupIssue(ctx, issue)
					_, statErr := os.Stat(info.Path)
					if !expired && i < 10 {
						if err != nil || !errors.Is(statErr, os.ErrNotExist) {
							t.Fatalf("candidate %d cleanup=%v stat=%v", i, err, statErr)
						}
					} else if !errors.Is(err, ErrWorkspacePreserved) || statErr != nil {
						t.Fatalf("candidate %d exceeded batch: cleanup=%v stat=%v", i, err, statErr)
					}
				}
			})
		}
	}
}
