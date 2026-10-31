package workspace

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestSourceVerificationUsesCallerLifetime(t *testing.T) {
	source := initSourceRepo(t)
	base := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	checkpointWrite(t, source, "source.txt", "reviewed source\n")
	runGit(t, source, "add", "source.txt")
	runGit(t, source, "commit", "-m", "reviewed source")
	head := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD"))
	capture, err := CaptureChangeSource(t.Context(), source, base, head)
	if err != nil {
		t.Fatal(err)
	}
	tree := strings.TrimSpace(runGit(t, source, "rev-parse", "HEAD^{tree}"))
	pack, err := captureIncorporationObjects(t.Context(), source, base, head, head)
	if err != nil {
		t.Fatal(err)
	}
	proof := tracker.LandingIncorporation{SourceBaseSHA: base, SourceHeadSHA: head, TargetHeadSHA: head, TargetTreeSHA: tree, MergeTreeSHA: tree}
	for _, kind := range []string{"source", "landing"} {
		for _, outcome := range []string{"slow valid", "cancelled", "deadline", "infrastructure"} {
			t.Run(kind+"/"+outcome, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
					defer cancel()
					calls := 0
					infrastructure := errors.New("verifier unavailable")
					run := func(commandCtx context.Context, command *exec.Cmd) ([]byte, error) {
						calls++
						if calls == 2 {
							switch outcome {
							case "cancelled":
								cancel()
							case "deadline":
								<-commandCtx.Done()
							case "infrastructure":
								return nil, infrastructure
							default:
								time.Sleep(16 * time.Second)
							}
						}
						if err := commandCtx.Err(); err != nil {
							return nil, err
						}
						if strings.Contains(strings.Join(command.Args, " "), "rev-parse") || strings.Contains(strings.Join(command.Args, " "), "merge-tree") {
							return []byte(tree + "\n"), nil
						}
						return nil, nil
					}
					var err error
					if kind == "source" {
						err = verifyChangeSource(ctx, capture.Source, capture.Bundle, run)
					} else {
						err = verifyLandingIncorporation(ctx, proof, pack, run)
					}
					var want error
					switch outcome {
					case "cancelled":
						want = context.Canceled
					case "deadline":
						want = context.DeadlineExceeded
					case "infrastructure":
						want = infrastructure
					}
					if !errors.Is(err, want) || errors.Is(err, ErrInvalidVerification) {
						t.Fatalf("verification = %v, want %v", err, want)
					}
				})
			})
		}
	}
}

func TestVerificationCommandFailureClassification(t *testing.T) {
	command := exec.CommandContext(t.Context(), "git", "cat-file", "-e", strings.Repeat("e", 40))
	command.Dir = t.TempDir()
	_, err := command.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("fixture exit = %v", err)
	}
	for _, test := range []struct {
		name, stderr string
		invalid      bool
	}{
		{"corrupt pack", "fatal: pack is corrupted (SHA1 mismatch)", true},
		{"missing object", "fatal: Not a valid object name", true},
		{"missing object canonical", "fatal: not a valid object name", true},
		{"disk full", "fatal: unable to create temporary file: No space left on device", false},
		{"permission", "fatal: unable to create temporary file: Permission denied", false},
		{"unknown failure", "fatal: unexpected failure", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			failure := *exit
			failure.Stderr = []byte(test.stderr)
			got := verificationCommandError(t.Context(), &failure, "invalid proof")
			if errors.Is(got, ErrInvalidVerification) != test.invalid {
				t.Fatalf("classification = %v", got)
			}
			if !test.invalid && !errors.Is(got, &failure) {
				t.Fatalf("lost infrastructure error: %v", got)
			}
		})
	}
}
