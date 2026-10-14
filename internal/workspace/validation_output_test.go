package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

func TestReviewCommandOutputSurvivesJSONByteBudget(t *testing.T) {
	if testing.Short() || runtime.GOOS == "windows" {
		t.Skip("git and shell subprocess integration")
	}
	const limit = 64 * 1024
	for _, test := range []struct {
		name               string
		output             string
		command            string
		exit               int
		truncated          bool
		reservationFailure bool
	}{
		{name: "reservation failure prevents command", command: "touch should-not-run", reservationFailure: true},
		{name: "short unicode", output: "verified ✓\n", command: "cat output.txt"},
		{name: "exact byte budget", output: strings.Repeat("a", limit-6) + "✓end", command: "cat output.txt"},
		{name: "three byte rune at tail boundary", output: strings.Repeat("✓", 32*1024) + "end\n", command: "cat output.txt", truncated: true},
		{name: "four byte rune at tail boundary", output: strings.Repeat("🙂", 24*1024) + "end\n", command: "cat output.txt", truncated: true},
		{name: "invalid bytes within budget", output: "before\xff\xffafter\n", command: "cat output.txt"},
		{name: "replacement crosses byte budget", output: strings.Repeat("a", limit-1) + "\xff", command: "cat output.txt", truncated: true},
		{name: "failed command preserves unicode tail", output: strings.Repeat("✓", 32*1024) + "end\n", command: "cat output.txt; exit 7", exit: 7, truncated: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			source := initSourceRepo(t)
			if err := os.WriteFile(filepath.Join(source, "output.txt"), []byte(test.output), 0o600); err != nil {
				t.Fatal(err)
			}
			runGit(t, source, "add", "output.txt")
			runGit(t, source, "commit", "-m", "validation output fixture")
			backend, err := NewLocalGit(LocalGitOptions{Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
			if err != nil {
				t.Fatal(err)
			}
			issue := Issue{Identifier: "review-output"}
			info, err := backend.Create(t.Context(), issue)
			if err != nil {
				t.Fatal(err)
			}
			head, err := backend.Head(t.Context(), info, issue)
			if err != nil {
				t.Fatal(err)
			}
			issue.PullRequestHeadSHA = strings.TrimSpace(head)
			reserved, registered, finished := false, false, false
			reservationErr := errors.New("reservation unavailable")
			ctx := WithValidationProcessOwner(t.Context(), func(_ context.Context, observed Info, _ Issue, scratch string) (ValidationProcess, error) {
				reserved = true
				if observed.Path != info.Path {
					t.Fatalf("reserved another workspace: %q", observed.Path)
				}
				if test.reservationFailure {
					return nil, reservationErr
				}
				if err := os.WriteFile(filepath.Join(scratch, "reserved"), nil, 0o600); err != nil {
					return nil, err
				}
				return validationProcessFixture{
					started: func(identity procgroup.Identity) error {
						if identity.PID <= 0 || identity.GroupID <= 0 || identity.StartedAt.IsZero() {
							t.Fatalf("invalid process identity: %+v", identity)
						}
						registered = true
						return nil
					},
					finished: func(result gate.CommandResult, cleanupErr error) error {
						finished = true
						if cleanupErr != nil {
							t.Fatalf("cleanup: %v", cleanupErr)
						}
						if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("scratch retained after successful cleanup: %v", err)
						}
						if result.ExitCode != test.exit {
							t.Fatalf("host completion exit=%d want=%d", result.ExitCode, test.exit)
						}
						return nil
					},
				}, nil
			})
			command := `test -f "$TMPDIR/reserved" || exit 91; ` + test.command
			receipt, err := backend.RunReviewCommand(ctx, info, issue, command)
			if test.reservationFailure {
				if !errors.Is(err, reservationErr) || !reserved || registered || finished {
					t.Fatalf("reservation failure execution: err=%v reserved=%t registered=%t finished=%t", err, reserved, registered, finished)
				}
				if _, err := os.Stat(filepath.Join(info.Path, "should-not-run")); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("command ran without reservation: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !reserved || !registered || !finished {
				t.Fatalf("incomplete host lifecycle: reserved=%t registered=%t finished=%t", reserved, registered, finished)
			}
			if !utf8.ValidString(receipt.Output) {
				t.Errorf("receipt output is not valid UTF-8")
			}
			encoded, err := json.Marshal(receipt)
			if err != nil {
				t.Fatal(err)
			}
			var decoded gate.CommandResult
			if err := json.Unmarshal(encoded, &decoded); err != nil {
				t.Fatal(err)
			}
			if len(decoded.Output) > limit {
				t.Errorf("transported output is %d bytes, exceeds %d", len(decoded.Output), limit)
			}
			if decoded.Output != receipt.Output || decoded.ExitCode != test.exit || decoded.Command != command || decoded.HeadSHA != issue.PullRequestHeadSHA || decoded.TreeSHA == "" {
				t.Errorf("transport changed validation output or command identity")
			}
			if decoded.OutputTruncated != test.truncated {
				t.Errorf("truncated = %v, want %v", decoded.OutputTruncated, test.truncated)
			}
			if test.truncated && !strings.HasPrefix(decoded.Output, "[earlier output truncated]\n") {
				t.Errorf("truncation marker missing")
			}
			if strings.HasSuffix(test.output, "end\n") && !strings.HasSuffix(decoded.Output, "end\n") {
				t.Errorf("useful command tail was discarded")
			}
			if !test.truncated && !strings.Contains(test.output, "\xff") && decoded.Output != test.output {
				t.Errorf("untruncated output changed")
			}
		})
	}
}

type validationProcessFixture struct {
	started  func(procgroup.Identity) error
	finished func(gate.CommandResult, error) error
}

func (p validationProcessFixture) Started(_ context.Context, identity procgroup.Identity) error {
	return p.started(identity)
}
func (p validationProcessFixture) Finished(_ context.Context, result gate.CommandResult, err error) error {
	return p.finished(result, err)
}
