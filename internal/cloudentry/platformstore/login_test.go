package platformstore

import (
	"errors"
	"sync"
	"testing"
	"time"
)

func TestLoginConsumption(t *testing.T) {
	db := template.Database(t)
	cfg := testConfig(t)
	first := repository(t, db, cfg)
	second := replicaRepository(t, first, cfg)
	for _, tc := range []struct {
		name        string
		expire      time.Duration
		wrongID     bool
		wantSuccess int
	}{
		{"one consumer", time.Minute, false, 1}, {"expired boundary", 0, false, 0}, {"past expiration", -time.Minute, false, 0}, {"wrong transaction", time.Minute, true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item := LoginTransaction{Hash: tc.name, ID: "transaction_" + tc.name, State: "state", Verifier: "private-verifier", InvitationToken: "private-invitation", SupportActor: "support@example.com", SupportSession: "support_session", SupportReason: "example reason", ExpiresAt: first.now().Add(time.Minute)}
			if err := first.CreateLogin(t.Context(), item); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(t.Context(), `UPDATE platform.transactions SET expires_at=$1 WHERE token_hash=$2`, first.now().Add(tc.expire), item.Hash); err != nil {
				t.Fatal(err)
			}
			var verifier, invitation string
			if err := db.QueryRowContext(t.Context(), `SELECT verifier,invitation_token FROM platform.transactions WHERE token_hash=$1`, item.Hash).Scan(&verifier, &invitation); err != nil {
				t.Fatal(err)
			}
			if verifier == item.Verifier || invitation == item.InvitationToken {
				t.Fatal("login secret stored in plaintext")
			}
			if tc.wrongID {
				item.ID = "wrong"
			}
			type outcome struct {
				item LoginTransaction
				err  error
			}
			results := make(chan outcome, 2)
			start := make(chan struct{})
			var workers sync.WaitGroup
			for _, r := range []*Repository{first, second} {
				workers.Go(func() {
					<-start
					got, err := r.ConsumeLogin(t.Context(), item.Hash, item.ID)
					results <- outcome{got, err}
				})
			}
			close(start)
			workers.Wait()
			close(results)
			successes := 0
			for result := range results {
				if result.err == nil {
					successes++
					if result.item.Verifier != item.Verifier || result.item.InvitationToken != item.InvitationToken || result.item.SupportReason != item.SupportReason {
						t.Fatalf("lost login fields: %+v", result.item)
					}
				} else if !errors.Is(result.err, ErrNoSession) {
					t.Fatal(result.err)
				}
			}
			if successes != tc.wantSuccess {
				t.Fatalf("successes=%d want=%d", successes, tc.wantSuccess)
			}
			if successes == 1 {
				if _, err := first.ConsumeLogin(t.Context(), item.Hash, item.ID); !errors.Is(err, ErrNoSession) {
					t.Fatalf("replay: %v", err)
				}
				if err := db.QueryRowContext(t.Context(), `SELECT verifier,invitation_token FROM platform.transactions WHERE token_hash=$1`, item.Hash).Scan(&verifier, &invitation); err != nil {
					t.Fatal(err)
				}
				if verifier != "" || invitation != "" {
					t.Fatal("consumed secrets retained")
				}
			}
		})
	}
}
