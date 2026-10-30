package runner

import (
	"context"
	"net"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

type recordingGoTestReceipts struct {
	mu       sync.Mutex
	receipts []gotestreceipt.Receipt
}

func (s *recordingGoTestReceipts) Attest(context.Context) (gotestreceipt.Attestation, error) {
	return gotestreceipt.Attestation{ID: "attestation", DigestKey: []byte(strings.Repeat("k", gotestreceipt.DigestKeyBytes))}, nil
}
func (s *recordingGoTestReceipts) Lookup(_ context.Context, key string) ([]gotestreceipt.Candidate, error) {
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

func (s *recordingGoTestReceipts) Record(_ context.Context, receipt gotestreceipt.Receipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = append(s.receipts, receipt)
	return nil
}

func TestSSHGoTestReceiptsReachTheCentralStore(t *testing.T) {
	store := &recordingGoTestReceipts{}
	central := &Runner{goTestReceipts: gotestreceipt.Config{Store: store, Wrapper: "/wrapper"}}
	left, right := net.Pipe()
	t.Cleanup(func() { left.Close(); right.Close() })
	centralPeer := NewSSHPeer(t.Context(), left, left, central.SSHCallbackHandler(RunRequest{}))
	t.Cleanup(centralPeer.Close)
	remotePeer := NewSSHPeer(t.Context(), right, right, nil)
	t.Cleanup(remotePeer.Close)
	remote := SSHGoTestReceipts{Peer: remotePeer}

	attestation, err := remote.Attest(t.Context())
	if err != nil || attestation.ID != "attestation" || string(attestation.DigestKey) != strings.Repeat("k", gotestreceipt.DigestKeyBytes) {
		t.Fatalf("attestation = %+v, %v", attestation, err)
	}
	receipt := gotestreceipt.Receipt{Key: strings.Repeat("a", 64), Package: "internal/example", Inputs: []string{"env\tHOME"}, Digest: strings.Repeat("b", 64)}
	if err := remote.Record(t.Context(), receipt); err != nil {
		t.Fatal(err)
	}
	candidates, err := remote.Lookup(t.Context(), receipt.Key)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 1 || candidates[0].Digest != receipt.Digest || !slices.Equal(candidates[0].Inputs, receipt.Inputs) {
		t.Fatalf("candidates = %+v, want the recorded receipt", candidates)
	}
	if !central.GoTestReceiptsEnabled() || (&Runner{}).GoTestReceiptsEnabled() {
		t.Fatal("receipt enablement does not follow the configured store")
	}
}
