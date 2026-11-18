package sshexec

import (
	"context"

	"github.com/digitaldrywood/detent/internal/gotestreceipt"
)

const (
	GoTestReceiptAttest = "go_test_receipts.attest"
	GoTestReceiptLookup = "go_test_receipts.lookup"
	GoTestReceiptRecord = "go_test_receipts.record"
)

type GoTestReceipts struct{ Peer Caller }

func (s GoTestReceipts) Attest(ctx context.Context) (gotestreceipt.Attestation, error) {
	var attestation gotestreceipt.Attestation
	err := s.Peer.Call(ctx, GoTestReceiptAttest, &attestation)
	return attestation, err
}

func (s GoTestReceipts) Lookup(ctx context.Context, key string) ([]gotestreceipt.Candidate, error) {
	var candidates []gotestreceipt.Candidate
	err := s.Peer.Call(ctx, GoTestReceiptLookup, &candidates, key)
	return candidates, err
}

func (s GoTestReceipts) Record(ctx context.Context, receipt gotestreceipt.Receipt) error {
	return s.Peer.Call(ctx, GoTestReceiptRecord, nil, receipt)
}
