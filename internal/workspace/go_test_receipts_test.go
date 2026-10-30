package workspace

import (
	"bytes"
	"context"
	"log/slog"
	"os"
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
