package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

type memoryGoTestReceipts struct {
	mu       sync.Mutex
	receipts []gotestreceipt.Receipt
}

func (s *memoryGoTestReceipts) Attest(context.Context) (gotestreceipt.Attestation, error) {
	return gotestreceipt.Attestation{ID: "attestation", DigestKey: []byte(strings.Repeat("k", gotestreceipt.DigestKeyBytes))}, nil
}
func (s *memoryGoTestReceipts) Lookup(_ context.Context, key string) ([]gotestreceipt.Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []gotestreceipt.Candidate
	for _, receipt := range s.receipts {
		if receipt.Key == key {
			candidates = append(candidates, gotestreceipt.Candidate{Inputs: receipt.Inputs, Digest: receipt.Digest})
		}
	}
	return candidates, nil
}

func (s *memoryGoTestReceipts) Record(_ context.Context, receipt gotestreceipt.Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = append(s.receipts, receipt)
	return nil
}

func TestRunReviewCommandSeparatesReceiptWorkspaces(t *testing.T) {
	if testing.Short() {
		t.Skip("subprocess Go toolchain fixture")
	}
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Go command fixture")
	}
	t.Setenv("CGO_ENABLED", "0")
	external := filepath.Join(t.TempDir(), "external-input")
	t.Setenv("DETENT_RECEIPT_FIXTURE", external)
	if err := os.WriteFile(external, []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := initSourceRepo(t)
	for name, content := range map[string]string{
		"go.mod":                 "module example.test/receipts\n\ngo 1.26.0\n",
		"pkg/testdata/input.txt": "ok",
		"pkg/pkg_test.go": `package pkg
import ("os"; "testing")
func TestInputs(t *testing.T) {
 local, err := os.ReadFile("testdata/input.txt")
 if err != nil || string(local) != "ok" { t.Fatalf("local input: %q %v", local, err) }
 external, err := os.ReadFile(os.Getenv("DETENT_RECEIPT_FIXTURE"))
 if err != nil || string(external) != "ready" { t.Fatalf("external input: %q %v", external, err) }
}
`,
	} {
		path := filepath.Join(source, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	runGit(t, source, "add", ".")
	runGit(t, source, "commit", "-m", "receipt fixture")
	var receiptLog bytes.Buffer
	backend, err := NewLocalGit(LocalGitOptions{Logger: slog.New(slog.NewTextHandler(&receiptLog, nil)), Root: filepath.Join(t.TempDir(), "workspaces"), SourceRoot: source, AutoBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	wrapper, err := gotestreceipt.Install(t.TempDir(), executable)
	if err != nil {
		t.Fatal(err)
	}
	store := &memoryGoTestReceipts{}
	ctx := gotestreceipt.WithContext(t.Context(), gotestreceipt.Config{Store: store, Wrapper: wrapper})
	run := func(identifier string) (int, string) {
		t.Helper()
		issue := Issue{Identifier: identifier}
		info, err := backend.Create(ctx, issue)
		if err != nil {
			t.Fatal(err)
		}
		head, err := backend.Head(ctx, info, issue)
		if err != nil {
			t.Fatal(err)
		}
		issue.PullRequestHeadSHA = strings.TrimSpace(head)
		result, err := backend.RunReviewCommand(ctx, info, issue, "go test ./...")
		if err != nil {
			t.Fatal(err)
		}
		return result.ExitCode, result.Output
	}

	steps := []struct {
		name       string
		identifier string
		external   string
		wantCode   int
		wantOutput string
	}{
		{name: "first workspace executes and records", identifier: "receipts-1", external: "ready", wantOutput: "0 of 1 test binaries reused, 1 recorded, 0 not recordable"},
		{name: "second workspace executes at its literal path", identifier: "receipts-2", external: "ready", wantOutput: "0 of 1 test binaries reused, 1 recorded, 0 not recordable"},
		{name: "changed external input executes again", identifier: "receipts-3", external: "changed", wantCode: 1, wantOutput: "external input"},
	}
	for _, step := range steps {
		if err := os.WriteFile(external, []byte(step.external), 0o600); err != nil {
			t.Fatal(err)
		}
		code, output := run(step.identifier)
		if code != step.wantCode || !strings.Contains(output, step.wantOutput) {
			t.Fatalf("%s: exit %d, want %d; output missing %q:\n%s\nreceipt log:\n%s", step.name, code, step.wantCode, step.wantOutput, output, receiptLog.String())
		}
	}
}

func TestGoTestReceiptSocketStaysInAssignedScratch(t *testing.T) {
	backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: initSourceRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	long := filepath.Join(t.TempDir(), strings.Repeat("scratch", 20))
	if err := os.Mkdir(long, 0o700); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name     string
		scratch  string
		want     bool
		unixOnly bool
	}{
		{name: "short scratch", scratch: t.TempDir(), want: true},
		{name: "scratch longer than a socket address", scratch: long, want: true, unixOnly: true},
		{name: "no assigned scratch runs without receipts", scratch: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.unixOnly && runtime.GOOS == "windows" {
				t.Skip("Windows has no per-thread working directory or descriptor path to shorten a socket address, so receipts there need a short scratch")
			}
			command := exec.CommandContext(t.Context(), "unused")
			ctx := gotestreceipt.WithContext(t.Context(), gotestreceipt.Config{Store: &memoryGoTestReceipts{}, Wrapper: "/wrapper"})
			session := backend.startGoTestReceipts(ctx, command, t.TempDir(), tt.scratch)
			if !tt.want {
				if session != nil {
					t.Fatal("receipt session started without an assigned scratch directory")
				}
				return
			}
			if session == nil {
				t.Fatal("receipt session unavailable")
			}
			defer func() {
				if _, err := session.Close(context.WithoutCancel(t.Context())); err != nil {
					t.Error(err)
				}
			}()
			environment := map[string]string{}
			for _, entry := range command.Environ() {
				if key, value, ok := strings.Cut(entry, "="); ok {
					environment[key] = value
				}
			}
			socket, err := filepath.EvalSymlinks(environment[gotestreceipt.SocketEnvironment])
			if err != nil {
				t.Fatal(err)
			}
			scratch, err := filepath.EvalSymlinks(tt.scratch)
			if err != nil {
				t.Fatal(err)
			}
			if relative, err := filepath.Rel(scratch, socket); err != nil || !filepath.IsLocal(relative) {
				t.Fatalf("socket %q is outside assigned scratch %q", socket, scratch)
			}
			var stdout, stderr bytes.Buffer
			probe := []string{filepath.Join(t.TempDir(), "probe.test")}
			gotestreceipt.RunWrapper(probe, func(name string) string { return environment[name] }, nil, &stdout, &stderr)
			if !strings.Contains(stderr.String(), "400 Bad Request") {
				t.Fatalf("receipt session did not answer through its socket: %s", stderr.String())
			}
		})
	}
}
