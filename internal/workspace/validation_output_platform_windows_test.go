//go:build windows

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestReview1973WindowsHostValidationWaitsForAcknowledgement(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "acknowledged"
		if denied {
			name = "denied"
		}
		t.Run(name, func(t *testing.T) {
			backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: initSourceRepo(t), AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "example-issue"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(info.Path, "provider-effect")
			rejected := errors.New("registration rejected")
			owner := &review1973ValidationProcess{acknowledge: func(identity procgroup.Identity) error {
				if identity.PID == 0 || identity.GroupID == 0 || identity.StartedAt.IsZero() {
					return errors.New("validation process has no suspended job identity")
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					return errors.New("validation ran before acknowledgement")
				}
				if denied {
					return rejected
				}
				return nil
			}}
			ctx := WithValidationProcessOwner(t.Context(), func(context.Context, Info, Issue, string) (ValidationProcess, error) {
				return owner, nil
			})
			result, err := backend.RunReviewCommand(ctx, info, issue, "echo executed>provider-effect")
			if denied {
				if !errors.Is(err, rejected) {
					t.Fatalf("validation registration error = %v", err)
				}
				if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("denied validation ran: %v", err)
				}
			} else if _, statErr := os.Stat(marker); err != nil || result.ExitCode != 0 || statErr != nil {
				t.Fatalf("acknowledged validation exit=%d error=%v marker=%v", result.ExitCode, err, statErr)
			}
		})
	}
}

type review1973ValidationProcess struct {
	acknowledge func(procgroup.Identity) error
}

func (p *review1973ValidationProcess) Started(_ context.Context, identity procgroup.Identity) error {
	return p.acknowledge(identity)
}

func (p *review1973ValidationProcess) Finished(context.Context, gate.CommandResult, error) error {
	return nil
}
