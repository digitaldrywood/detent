package store

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/mutation"
)

// Catch duplicate execution after response loss/restart, competing claims,
// changed payloads, and collisions across actor/organization/operation.
func TestOperatorMutationDurableRetry(t *testing.T) {
	if testing.Short() {
		t.Skip("durable SQLite integration")
	}

	t.Parallel()
	path := filepath.Join(t.TempDir(), "mutation.db")
	backend, err := Open(t.Context(), Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := backend.Close(); err != nil {
			t.Error(err)
		}
	}()
	records := backend.(OperatorMutations)
	base := mutation.Metadata{PrincipalID: "actor", OrganizationID: "org", ProjectID: "project", Action: "file_issue", Source: "mcp", Confirmation: "none", CorrelationID: "correlation"}
	const sentinel = "credential-invitation-prompt-comment-body-support-billing-sensitive-sentinel"
	m, err := base.Bind("business-key", map[string]string{"body": sentinel})
	if err != nil {
		t.Fatal(err)
	}
	var claims atomic.Int32
	var wg sync.WaitGroup
	for range 12 {
		wg.Go(func() {
			claimed, err := records.ReserveOperatorMutation(t.Context(), m)
			if err != nil {
				t.Error(err)
			}
			if claimed {
				claims.Add(1)
			}
		})
	}
	wg.Wait()
	if claims.Load() != 1 {
		t.Fatalf("claims=%d", claims.Load())
	}
	pending, found, err := records.OperatorMutation(t.Context(), m)
	if err != nil || !found || pending.Outcome != "pending" {
		t.Fatalf("pending=%+v %t %v", pending, found, err)
	}

	sqliteBackend := backend.(*sqliteStore)
	if _, err := sqliteBackend.db.ExecContext(t.Context(), `UPDATE workflow_phase_events SET retry_identity=NULL,input_hash=NULL,correlation_id=NULL,projection_ready=NULL`); err != nil {
		t.Fatal(err)
	}
	if receipt, found, err := records.OperatorMutation(t.Context(), m); err != nil || !found || receipt.CorrelationID != m.CorrelationID {
		t.Fatalf("legacy receipt=%+v found=%t err=%v", receipt, found, err)
	}
	if reserved, err := records.ReserveOperatorMutation(t.Context(), m); err != nil || reserved {
		t.Fatalf("legacy duplicate reservation=%t err=%v", reserved, err)
	}
	if claimed, err := records.ClaimOperatorMutation(t.Context(), m); err != nil || !claimed {
		t.Fatalf("execution claim=%t %v", claimed, err)
	}
	if claimed, err := records.ClaimOperatorMutation(t.Context(), m); err != nil || claimed {
		t.Fatalf("duplicate execution claim=%t %v", claimed, err)
	}
	m.ResourceID = "issue-id"
	if err := records.CompleteOperatorMutation(t.Context(), OperatorReceipt{Metadata: m, Outcome: "succeeded", Identifier: "project#1", URL: "https://example.test/1"}); err != nil {
		t.Fatal(err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}
	backend, err = Open(t.Context(), Config{Path: path})
	if err != nil {
		t.Fatal(err)
	}
	records = backend.(OperatorMutations)
	receipt, found, err := records.OperatorMutation(t.Context(), m)
	if err != nil || !found || receipt.ResourceID != "issue-id" || receipt.Outcome != "succeeded" || receipt.CompletedAt.IsZero() {
		t.Fatalf("reconnect=%+v %t %v", receipt, found, err)
	}
	if claimed, err := records.ClaimOperatorMutation(t.Context(), m); err != nil || claimed {
		t.Fatalf("sequential claim=%t %v", claimed, err)
	}
	raw, _ := json.Marshal(receipt)
	if strings.Contains(string(raw), sentinel) || strings.Contains(string(raw), "business-key") {
		t.Fatal("sensitive input stored in receipt")
	}
	for _, tc := range []struct {
		name     string
		modify   func(*mutation.Metadata)
		input    string
		conflict bool
	}{
		{"changed payload", func(*mutation.Metadata) {}, "changed", true},
		{"different actor", func(m *mutation.Metadata) { m.PrincipalID = "other" }, sentinel, false},
		{"different organization", func(m *mutation.Metadata) { m.OrganizationID = "other" }, sentinel, false},
		{"different operation", func(m *mutation.Metadata) { m.Action = "comment" }, sentinel, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := base
			tc.modify(&next)
			next, err := next.Bind("business-key", map[string]string{"body": tc.input})
			if err != nil {
				t.Fatal(err)
			}
			_, found, err := records.OperatorMutation(t.Context(), next)
			if tc.conflict {
				if !found || !errors.Is(err, mutation.ErrConflict) {
					t.Fatalf("conflict=%t %v", found, err)
				}
			} else {
				if found || err != nil {
					t.Fatalf("boundary=%t %v", found, err)
				}
				if claimed, err := records.ReserveOperatorMutation(t.Context(), next); !claimed || err != nil {
					t.Fatalf("boundary claim=%t %v", claimed, err)
				}
			}
		})
	}
	for _, identity := range []string{"missing-hash-one", "missing-hash-two"} {
		missing := base
		missing.RetryIdentity = identity
		if reserved, err := records.ReserveOperatorMutation(t.Context(), missing); err != nil || !reserved {
			t.Fatalf("reserve missing hash=%t err=%v", reserved, err)
		}
		for _, stage := range []string{"typed", "legacy", "backfilled"} {
			sqliteBackend := backend.(*sqliteStore)
			if stage == "legacy" {
				if _, err := sqliteBackend.db.ExecContext(t.Context(), `UPDATE workflow_phase_events SET retry_identity=NULL,input_hash=NULL,correlation_id=NULL,projection_ready=NULL WHERE retry_identity=?`, identity); err != nil {
					t.Fatal(err)
				}
			}
			if stage == "backfilled" {
				if count, err := sqliteBackend.BackfillTypedProjections(t.Context()); err != nil || count != 1 {
					t.Fatalf("receipt backfill=%d err=%v", count, err)
				}
			}
			if claimed, err := records.ClaimOperatorMutation(t.Context(), missing); err != nil || claimed {
				t.Fatalf("%s NULL hash authorized claim=%t err=%v", stage, claimed, err)
			}
			if err := records.CompleteOperatorMutation(t.Context(), OperatorReceipt{Metadata: missing, Outcome: "succeeded"}); !errors.Is(err, mutation.ErrUncertain) {
				t.Fatalf("%s NULL hash authorized completion: %v", stage, err)
			}
		}
	}

}
