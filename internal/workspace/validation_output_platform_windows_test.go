//go:build windows

package workspace

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestReview1973WindowsHostValidationWaitsForAcknowledgement(t *testing.T) {
	for _, denied := range []bool{false, true} {
		name := "acknowledged"
		if denied {
			name = "denied"
		}
		t.Run(name, func(t *testing.T) {
			backend, err := NewLocalGit(LocalGitOptions{Root: testenv.TempDir(t), SourceRoot: initSourceRepo(t), AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "example-issue", AttemptWorkspace: AttemptWorkspace{Organization: "org_example", Project: "prj_example", Attempt: "attempt_11111111111111111111111111111111"}}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			issue.PullRequestHeadSHA = strings.TrimSpace(runGit(t, info.Path, "rev-parse", "HEAD"))
			marker := filepath.Join(t.TempDir(), "provider-effect")
			rejected := errors.New("registration rejected")
			registrations := 0
			ctx := WithValidationProcessOwner(WithAttemptWorkspace(t.Context(), issue.AttemptWorkspace), func(_ context.Context, _ Info, _ Issue, scratch string) (ValidationProcess, error) {
				return &review1973ValidationProcess{acknowledge: func(identity procgroup.Identity) error {
					if identity.PID == 0 || identity.GroupID == 0 || identity.StartedAt.IsZero() {
						return errors.New("process has no suspended job identity")
					}
					if scratch == "" {
						return nil
					}
					registrations++
					if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
						return errors.New("validation ran before acknowledgement")
					}
					if denied {
						return rejected
					}
					return nil
				}}, nil
			})
			ctx = WithProcessSource(ctx, func(context.Context, string) ([]int, error) { return nil, nil })
			result, err := backend.RunReviewCommand(ctx, info, issue, `echo executed>"`+marker+`"`)
			if registrations == 0 {
				t.Fatalf("validation never reached registration: %v", err)
			}
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
