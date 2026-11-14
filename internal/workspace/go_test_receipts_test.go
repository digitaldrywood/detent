package workspace

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
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

func TestGoTestReceiptSocketStaysInAssignedScratch(t *testing.T) {
	t.Parallel()
	backend, err := NewLocalGit(LocalGitOptions{Root: t.TempDir(), SourceRoot: initSourceRepo(t)})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name    string
		scratch string
		want    bool
	}{
		{name: "short scratch", scratch: t.TempDir(), want: true},
		{name: "no assigned scratch runs without receipts", scratch: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			testGoTestReceiptSocket(t, backend, tt.scratch, tt.want)
		})
	}
}

func testGoTestReceiptSocket(t *testing.T, backend *LocalGit, assignedScratch string, want bool) {
	t.Helper()
	command := exec.CommandContext(t.Context(), "unused")
	ctx := gotestreceipt.WithContext(t.Context(), gotestreceipt.Config{Store: &memoryGoTestReceipts{}, Wrapper: "/wrapper"})
	session := backend.startGoTestReceipts(ctx, command, t.TempDir(), assignedScratch)
	if !want {
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
	scratch, err := filepath.EvalSymlinks(assignedScratch)
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
}
