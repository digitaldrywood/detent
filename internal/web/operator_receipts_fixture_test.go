package web_test

import (
	"context"
	"sync"

	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/store"
)

type operatorReceiptStoreFixture struct {
	authKeyStoreFixture
	receiptsMu sync.Mutex
	claimed    map[string]bool
	receipts   map[string]store.OperatorReceipt
}

func newOperatorReceiptStoreFixture() *operatorReceiptStoreFixture {
	return &operatorReceiptStoreFixture{authKeyStoreFixture: authKeyStoreFixture{keys: make(map[string]store.APIKey)}, receipts: make(map[string]store.OperatorReceipt), claimed: make(map[string]bool)}
}

func (s *operatorReceiptStoreFixture) OperatorMutation(ctx context.Context, m mutation.Metadata) (store.OperatorReceipt, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.OperatorReceipt{}, false, err
	}
	s.receiptsMu.Lock()
	defer s.receiptsMu.Unlock()
	receipt, found := s.receipts[m.RetryIdentity]
	if found && receipt.InputHash != m.InputHash {
		return store.OperatorReceipt{}, false, mutation.ErrConflict
	}
	return receipt, found, nil
}

func (s *operatorReceiptStoreFixture) ReserveOperatorMutation(ctx context.Context, m mutation.Metadata) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.receiptsMu.Lock()
	defer s.receiptsMu.Unlock()
	if receipt, found := s.receipts[m.RetryIdentity]; found {
		if receipt.InputHash != m.InputHash {
			return false, mutation.ErrConflict
		}
		return false, nil
	}
	s.receipts[m.RetryIdentity] = store.OperatorReceipt{Metadata: m, Outcome: "pending"}
	return true, nil
}

func (s *operatorReceiptStoreFixture) ClaimOperatorMutation(ctx context.Context, m mutation.Metadata) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	s.receiptsMu.Lock()
	defer s.receiptsMu.Unlock()
	receipt, found := s.receipts[m.RetryIdentity]
	if !found {
		return false, mutation.ErrUncertain
	}
	if receipt.InputHash != m.InputHash {
		return false, mutation.ErrConflict
	}
	if receipt.Outcome != "pending" || receipt.CorrelationID != m.CorrelationID || s.claimed[m.RetryIdentity] {
		return false, nil
	}
	s.claimed[m.RetryIdentity] = true
	return true, nil
}

func (s *operatorReceiptStoreFixture) CompleteOperatorMutation(ctx context.Context, receipt store.OperatorReceipt) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.receiptsMu.Lock()
	defer s.receiptsMu.Unlock()
	existing, found := s.receipts[receipt.RetryIdentity]
	if !found {
		return mutation.ErrUncertain
	}
	if existing.InputHash != receipt.InputHash {
		return mutation.ErrConflict
	}
	s.receipts[receipt.RetryIdentity] = receipt
	return nil
}
