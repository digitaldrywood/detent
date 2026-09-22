package workspace

import (
	"context"
	"errors"
	"fmt"
	"os"
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
	backend, err := NewFilesystem(FilesystemOptions{Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	issue := Issue{Identifier: "repo#1", ID: "1"}
	release, err := backend.Use(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	info, err := backend.Create(t.Context(), issue)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := backend.CleanupIssue(t.Context(), issue); !errors.Is(err, ErrWorkspacePreserved) {
		t.Fatalf("cleanup=%v", err)
	}
	if _, err := os.Stat(info.Path); err != nil {
		t.Fatalf("active workspace removed: %v", err)
	}
}

func TestFilesystemCleanupBatch(t *testing.T) {
	for _, count := range []int{0, 1, 10, 11, 50} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			backend, err := NewFilesystem(FilesystemOptions{Root: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			ctx := WithCleanupBatch(t.Context(), 10)
			for i := range count {
				issue := Issue{Identifier: fmt.Sprintf("repo#%d", i), ID: strconv.Itoa(i)}
				info, err := backend.Create(t.Context(), issue)
				if err != nil {
					t.Fatal(err)
				}
				_, err = backend.CleanupIssue(ctx, issue)
				_, statErr := os.Stat(info.Path)
				if i < 10 {
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
