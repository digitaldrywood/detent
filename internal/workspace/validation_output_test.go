package workspace

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/tracker"
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
		selection          *ReviewOutputRange
		wantDiagnostics    []string
	}{
		{
			name: "separated failures survive both limits",
			output: "assertion setup\nAssertionError: expected range\nexpected selection\nactual null\n--- FAIL: TestFirst (0.01s)\nTOKEN=private-assignment\n" +
				strings.Repeat("ok example.org/passing\n", 4000) +
				"--- FAIL: TestSecond/世界 (0.02s)\nFAIL\texample.org/second\t1s\n" +
				strings.Repeat("ok example.org/passing\n", 4000) +
				"panic: test timed out after 1m0s\nrunning tests:\n\tTestThird (1m0s)\n\tTestFourth (1m0s)\n\n" +
				strings.Repeat("goroutine stack\n", 4000) +
				"internal/example.go:12:3: unchecked result (errcheck)\nfatal error: concurrent map writes\n" +
				strings.Repeat("ok example.org/passing\n", 4000) + "FAIL\texample.org/third\t61s\nend\n",
			command: "cat output.txt; exit 1", exit: 1, truncated: true,
			wantDiagnostics: []string{"assertion setup", "AssertionError: expected range", "expected selection", "actual null", "--- FAIL: TestFirst (0.01s)\n", "--- FAIL: TestSecond/世界 (0.02s)\n", "FAIL\texample.org/second\t1s\n", "panic: test timed out after 1m0s\n", "running tests:\n\tTestThird (1m0s)\n\tTestFourth (1m0s)\n", "internal/example.go:12:3: unchecked result (errcheck)\n", "fatal error: concurrent map writes\n", "FAIL\texample.org/third\t61s\n"},
		},
		{
			name: "secret assignment crosses retention boundaries",
			output: "panic: example failure\n" + strings.Repeat("context line\n", 20) +
				strings.Repeat("ok example.org/passing\n", 4000) + "TOKEN=" + strings.Repeat("private-fragment", 12000) + "\nend\n",
			command: "cat output.txt; exit 1", exit: 1, truncated: true,
			wantDiagnostics: []string{"panic: example failure", "TOKEN=[redacted]"},
		},
		{name: "reservation failure prevents command", command: "touch should-not-run", reservationFailure: true},
		{name: "bounded source capture before host completion", output: strings.Repeat("a", 2*1024*1024), command: "cat output.txt", selection: &ReviewOutputRange{Offset: 1024 * 1024, Length: 1}, truncated: true},
		{name: "paged failure retains diagnostics", output: strings.Repeat("a", 2*1024*1024), command: "cat output.txt; echo source-failure >&2; exit 7", exit: 7, selection: &ReviewOutputRange{Offset: 1024 * 1024, Length: 1}, truncated: true},
		{name: "paged diagnostics are bounded", output: strings.Repeat("a", 2*1024*1024), command: "echo source-failure >&2; cat output.txt >&2; exit 7", exit: 7, selection: &ReviewOutputRange{Offset: 1024 * 1024, Length: 1}, truncated: true},
		{name: "short unicode", output: "verified ✓\n", command: "cat output.txt"},
		{name: "exact byte budget", output: strings.Repeat("a", limit-6) + "✓end", command: "cat output.txt"},
		{name: "three byte rune at tail boundary", output: strings.Repeat("✓", 32*1024) + "end\n", command: "cat output.txt", truncated: true},
		{name: "four byte rune at tail boundary", output: strings.Repeat("🙂", 24*1024) + "end\n", command: "cat output.txt", truncated: true},
		{name: "invalid bytes within budget", output: "before\xff\xffafter\n", command: "cat output.txt"},
		{name: "replacement crosses byte budget", output: strings.Repeat("a", limit-1) + "\xff", command: "cat output.txt", truncated: true},
		{name: "failed command preserves unicode tail", output: strings.Repeat("✓", 32*1024) + "end\n", command: "cat output.txt; exit 7", exit: 7, truncated: true},
		{
			name: "terminal make failure preserves preceding diagnostic",
			output: strings.Repeat(" PASS frontend suite 世界\n", 8000) +
				"./scripts/check-repository.sh\nRepository schema mismatch 世界\nmigrations/example.sql: missing delivery column\nexpected column: delivery_id\nactual column: legacy_id\nTOKEN=private-assignment\n/home/private/project/build.log\nmake: *** [check-repository] Error 2\nend\n",
			command: "cat output.txt; exit 2", exit: 2, truncated: true,
			wantDiagnostics: []string{"./scripts/check-repository.sh", "Repository schema mismatch 世界", "migrations/example.sql: missing delivery column", "expected column: delivery_id", "actual column: legacy_id", "make: *** [check-repository] Error 2"},
		},
		{
			name: "early migration repository stage survives public receipt",
			output: strings.Repeat("ok repository setup\n", 300) + "./scripts/check-migration-order.sh dev\nMIGRATION ORDERING CONFLICT DETECTED!\nmigrations/20261017090000_example.sql is below dev migration 20261017100000_other.sql\ncheck-fast-stage stage=repository exit_status=1\n" +
				"TOKEN=private-assignment\n/home/private/project/build.log\n" +
				strings.Repeat(" PASS frontend suite 世界\n", 8000) + "make: *** [check-fast] Error 2\nend\n",
			command: "cat output.txt; exit 2", exit: 2, truncated: true,
			wantDiagnostics: []string{"./scripts/check-migration-order.sh dev", "MIGRATION ORDERING CONFLICT DETECTED!", "20261017090000_example.sql", "stage=repository exit_status=1"},
		},
		{
			name: "early make target survives public receipt",
			output: strings.Repeat("ok repository setup\n", 300) + "make[1]: *** [check-migrations] Error 1\n" +
				"TOKEN=private-assignment\n/home/private/project/build.log\n" +
				strings.Repeat(" PASS frontend suite 世界\n", 8000) + "make: *** [check-fast] Error 2\nend\n",
			command: "cat output.txt; exit 2", exit: 2, truncated: true,
			wantDiagnostics: []string{"make[1]: *** [check-migrations] Error 1"},
		},
		{
			name: "early stage exit survives public receipt",
			output: strings.Repeat("ok repository setup\n", 300) + "[check-fast-stage] stage=repository command=check-repository exit_status=2\n" +
				"TOKEN=private-assignment\n/home/private/project/build.log\n" +
				strings.Repeat(" PASS frontend suite 世界\n", 8000) + "make: *** [check-fast] Error 2\nend\n",
			command: "cat output.txt; exit 2", exit: 2, truncated: true,
			wantDiagnostics: []string{"stage=repository command=check-repository exit_status=2"},
		},
		{
			name: "early TypeScript compiler survives public receipt",
			output: strings.Repeat("ok repository setup\n", 300) + "src/widget.ts(12,7): error TS2322: Type 'string' is not assignable to type 'number'.\n" +
				"TOKEN=private-assignment\n/home/private/project/build.log\n" +
				strings.Repeat(" PASS frontend suite 世界\n", 8000) + "make: *** [check-fast] Error 2\nend\n",
			command: "cat output.txt; exit 2", exit: 2, truncated: true,
			wantDiagnostics: []string{"src/widget.ts(12,7): error TS2322"},
		},
		{
			name: "early bundler survives public receipt",
			output: strings.Repeat("ok repository setup\n", 300) + "✘ [ERROR] Could not resolve \"./missing\"\n    src/widget.tsx:12:7\n" +
				"TOKEN=private-assignment\n/home/private/project/build.log\n" +
				strings.Repeat(" PASS frontend suite 世界\n", 8000) + "make: *** [check-fast] Error 2\nend\n",
			command: "cat output.txt; exit 2", exit: 2, truncated: true,
			wantDiagnostics: []string{"[ERROR] Could not resolve \"./missing\"", "src/widget.tsx:12:7"},
		},
		{
			name: "early npm build survives public receipt",
			output: strings.Repeat("ok repository setup\n", 300) + "npm error code ELIFECYCLE\nnpm error command failed: npm run build\n" +
				"TOKEN=private-assignment\n/home/private/project/build.log\n" +
				strings.Repeat(" PASS frontend suite 世界\n", 8000) + "make: *** [check-fast] Error 2\nend\n",
			command: "cat output.txt; exit 2", exit: 2, truncated: true,
			wantDiagnostics: []string{"npm error code ELIFECYCLE", "npm error command failed: npm run build"},
		},
		{
			name: "early Go timeout survives public receipt",
			output: strings.Repeat("ok example.org/build\n", 100) +
				"panic: test timed out after 1m0s\nrunning tests:\n\tTestWorker/世界 (1m0s)\n" +
				"TOKEN=private-assignment\n/home/private/project/worker_test.go:42\n" +
				strings.Repeat("goroutine 99 [select]: diagnostic 世界\n", 4000) + "FAIL example.org/worker 61s\nend\n",
			command: "cat output.txt; exit 7", exit: 7, truncated: true,
			wantDiagnostics: []string{"panic: test timed out after 1m0s", "TestWorker/世界", "FAIL example.org/worker 61s"},
		},
		{
			name: "early colored frontend failure survives public receipt",
			output: "\x1b[31m FAIL \x1b[0m packages/ui/selection.test.tsx > selects a range\n" +
				"AssertionError: expected range but received null\n\x1b[31mTOK\x1b[0mEN=private-assignment\n" +
				"/home/private/project/selection.test.tsx:42\n" +
				strings.Repeat(" PASS later suite 世界\n", 4000) + "Tests 1 failed | 4000 passed\nend\n",
			command: "cat output.txt; exit 7", exit: 7, truncated: true,
			wantDiagnostics: []string{"packages/ui/selection.test.tsx > selects a range", "AssertionError: expected range but received null", "Tests 1 failed | 4000 passed"},
		},
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
						if test.selection == nil && strings.Contains(result.Output, "private") {
							t.Fatal("host completion retained unredacted output")
						}
						if cleanupErr != nil {
							t.Fatalf("cleanup: %v", cleanupErr)
						}
						if _, err := os.Stat(scratch); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("scratch retained after successful cleanup: %v", err)
						}
						if test.selection != nil && len(result.Output) > 64*1024 {
							t.Fatalf("host completion retained %d bytes for a %d-byte page", len(result.Output), test.selection.Length)
						}
						if result.ExitCode != test.exit {
							t.Fatalf("host completion exit=%d want=%d", result.ExitCode, test.exit)
						}
						return nil
					},
				}, nil
			})
			if test.selection != nil {
				ctx = WithReviewOutputRange(ctx, *test.selection)
			}
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
			if test.selection != nil {
				if test.exit == 0 {
					prefix := fmt.Sprintf("[source bytes 1048576:1048577 of %d; next_offset=1048577; eof=false]\n", len(test.output))
					if decoded.Output != prefix+"a" {
						t.Fatalf("wrong source page: %q", decoded.Output)
					}
				} else if !strings.Contains(decoded.Output, "source-failure") {
					t.Fatalf("failure lost stderr: %q", decoded.Output)
				}
				return
			}
			if test.truncated && test.wantDiagnostics == nil && !strings.HasPrefix(decoded.Output, "[earlier output truncated]\n") {
				t.Errorf("truncation marker missing")
			}
			if test.wantDiagnostics != nil {
				if !strings.Contains(decoded.Output, "output truncated]") {
					t.Errorf("diagnostic truncation marker missing")
				}
				public := tracker.PublicGateResult(decoded)
				for _, want := range test.wantDiagnostics {
					if !strings.Contains(decoded.Output, want) || !strings.Contains(public.Output, want) {
						t.Errorf("original diagnostic missing from command or public receipt: %q", want)
					}
				}
				if strings.Contains(decoded.Output, "private") || strings.Contains(public.Output, "private") || strings.Contains(public.Output, "\x1b") || !strings.Contains(public.Output, "[redacted]") {
					t.Errorf("public receipt did not redact normalized diagnostic")
				}
				if len(public.Output) > tracker.NativeFinalizationTextLimit || !utf8.ValidString(public.Output) || !public.OutputTruncated || public.ExitCode != decoded.ExitCode || public.HeadSHA != decoded.HeadSHA || public.TreeSHA != decoded.TreeSHA {
					t.Errorf("public receipt lost byte bounds, truncation or command identity")
				}
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

func TestReviewOutputRangeRetainsBoundedMemory(t *testing.T) {
	chunk := []byte(strings.Repeat("x", 32*1024))
	measurement := testing.Benchmark(func(b *testing.B) {
		for range b.N {
			capture := reviewOutputCapture{selection: ReviewOutputRange{Offset: 32 * 1024 * 1024, Length: ReviewSourcePageBytes}}
			for range 2048 {
				if _, err := capture.Write(chunk); err != nil {
					b.Fatal(err)
				}
			}
			output, truncated := capture.result()
			if capture.total != 64*1024*1024 || !truncated || !strings.HasSuffix(output, strings.Repeat("x", ReviewSourcePageBytes)) {
				b.Fatal("streamed page lost source bytes or metadata")
			}
		}
	})
	if measurement.AllocedBytesPerOp() > 256*1024 {
		t.Fatalf("64 MiB stream allocated %d bytes for a 16 KiB page", measurement.AllocedBytesPerOp())
	}
}

func TestReviewOutputUTF8Pages(t *testing.T) {
	for _, fixture := range []struct {
		name, content string
		offset        int64
		length        int
	}{
		{"three byte page boundary", strings.Repeat("a", 16383) + "✓end", 0, 16384},
		{"four byte page boundary", strings.Repeat("a", 16383) + "🙂end", 0, 16384},
		{"one byte request", "✓🙂end", 0, 1},
		{"offset within rune", "✓🙂end", 1, 1},
	} {
		for _, chunk := range []int{1, 2, 32768} {
			t.Run(fmt.Sprintf("%s/chunk%d", fixture.name, chunk), func(t *testing.T) {
				var observed strings.Builder
				for offset := fixture.offset; ; {
					capture := reviewOutputCapture{selection: ReviewOutputRange{Offset: offset, Length: fixture.length}}
					for start := 0; start < len(fixture.content); start += chunk {
						if _, err := capture.Write([]byte(fixture.content[start:min(start+chunk, len(fixture.content))])); err != nil {
							t.Fatal(err)
						}
					}
					output, _ := capture.result()
					header, content, ok := strings.Cut(output, "\n")
					var start, end, total, next int64
					var eof bool
					if _, err := fmt.Sscanf(header, "[source bytes %d:%d of %d; next_offset=%d; eof=%t]", &start, &end, &total, &next, &eof); err != nil {
						t.Fatal(err)
					}
					if !ok || !utf8.ValidString(content) || end-start != int64(len(content)) || total != int64(len(fixture.content)) || next != end || end <= offset || len(content) > ReviewSourcePageBytes {
						t.Fatalf("invalid UTF-8 page: %q", output)
					}
					observed.WriteString(content)
					if eof {
						break
					}
					offset = next
				}
				if observed.String() != fixture.content {
					t.Fatalf("following next_offset lost source characters: got %q", observed.String())
				}
			})
		}
	}
}
