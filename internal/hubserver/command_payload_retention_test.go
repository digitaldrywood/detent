package hubserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCommandPayloadRetentionNeverRepeatsEffects(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }, retentionTimeout: frozenRetentionTimeout})
	s := f.service
	if err := s.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := s.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.database.db.ExecContext(t.Context(), `INSERT INTO organizations(id,name,created_at) VALUES('org_example_second','Second example',?)`, formatHubTime(start)); err != nil {
		t.Fatal(err)
	}
	for _, organization := range []tracker.OrganizationID{f.project.OrganizationID, "org_example_second"} {
		for _, operation := range []string{"POST /example", "PUT /example", "PATCH /example", "DELETE /example", "POST /example/conversations", "POST /example/conversations/c/link", "mcp update_runner_routing", "runner_routing example", "runner_capacity example", "runner_update example", "urgent_runner_update", "apply_local_project_policy example project", "drain_local_project example project", "detach_local_project example project", "resume_local_project example project", "attachment.record example"} {
			t.Run(string(organization)+"/"+operation, func(t *testing.T) {
				clock.Store(start.UnixNano())
				ctx := s.contentContext(t.Context())
				input := map[string]string{"effect": "example"}
				hash, err := nativeCommandHash(input)
				if err != nil {
					t.Fatal(err)
				}
				scope := nativeScope{organization: organization, credential: apiCredential{ID: defaultHubOperatorID}}
				raw := `{"output":"` + strings.Repeat("x", 64<<10) + `"}`
				command := hostedCommand{organization: string(organization), actor: scope.credential.ID, operation: operation, key: "external", input: input}
				effects := 0
				run := func() ([]byte, error) {
					claimed, response, err := s.claimHostedOperation(ctx, command)
					if err != nil || !claimed {
						return response, err
					}
					effects++
					return s.completeHostedOperation(ctx, command, json.RawMessage(raw))
				}
				if _, err := run(); err != nil {
					t.Fatal(err)
				}
				if _, err := prepareMeteredContent(ctx, string(organization), []byte(raw), !strings.HasPrefix(operation, "attachment.")); err != nil {
					t.Fatal(err)
				}
				tx, err := s.database.db.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer tx.Rollback()
				if _, err := insertNativeCommandReceipt(ctx, tx, organization, scope.credential.ID, operation+" example-session", "native", hash, raw, formatHubTime(start)); err != nil {
					t.Fatal(err)
				}
				if err := tx.Commit(); err != nil {
					t.Fatal(err)
				}
				for _, state := range []struct {
					key, body string
					pending   any
				}{{"pending", `{"pending":true}`, true}, {"uncertain", hostedPendingCommand, nil}, {"legacy", raw, nil}} {
					if _, err := s.database.db.ExecContext(ctx, `INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at,response_pending) VALUES(?,?,?,?,?,?,?,?)`, organization, scope.credential.ID, operation, state.key, hash, state.body, formatHubTime(start), state.pending); err != nil {
						t.Fatal(err)
					}
				}
				for _, age := range []time.Duration{0, 24 * time.Hour, 24*time.Hour + time.Second} {
					clock.Store(start.Add(age).UnixNano())
					if err := s.database.populateRunnerFacts(ctx); err != nil {
						t.Fatal(err)
					}
					if err := s.retainCommandPayloads(ctx, s.config.now()); err != nil {
						t.Fatal(err)
					}
					response, err := run()
					if age <= 24*time.Hour {
						if err != nil || string(response) != raw {
							t.Fatalf("external replay lost body: %v", err)
						}
					} else {
						var expired *nativeError
						if !errors.As(err, &expired) || expired.Code != "idempotency_expired" {
							t.Errorf("external replay did not return tombstone: %v", err)
						}
					}
					if effects != 1 {
						t.Fatalf("external effect repeated %d times", effects)
					}
					for _, key := range []string{"external", "native", "legacy"} {
						var inline, object string
						storedOperation := operation
						if key == "native" {
							storedOperation += " example-session"
						}
						if err := s.database.reader.QueryRowContext(ctx, `SELECT response_json,response_object_key FROM native_commands WHERE organization_id=? AND actor_id=? AND operation=? AND command_key=?`, organization, scope.credential.ID, storedOperation, key).Scan(&inline, &object); err != nil {
							t.Fatalf("age=%s key=%s lost idempotency tombstone: %v", age, key, err)
						}
						if key != "legacy" && len(inline) > 2 {
							t.Errorf("age=%s key=%s retained %d inline bytes", age, key, len(inline))
						}
						response, found, err := nativeCommandReceipt(ctx, s.database.reader, scope, operation, key, hash)
						if age <= 24*time.Hour {
							if err != nil || !found || response != raw {
								t.Fatalf("age=%s key=%s replay lost body: found=%t error=%v", age, key, found, err)
							}
						} else {
							var expired *nativeError
							if inline != "{}" || object != "" || !found || !errors.As(err, &expired) || expired.Code != "idempotency_expired" || expired.status != http.StatusGone {
								t.Errorf("key=%s expired body/tombstone: inline=%d object=%q found=%t error=%v", key, len(inline), object, found, err)
							}
						}
					}
					for _, key := range []string{"pending", "uncertain"} {
						unresolved := command
						unresolved.key = key
						claimed, body, err := s.claimHostedOperation(ctx, unresolved)
						if claimed || key == "uncertain" && !errors.Is(err, mutation.ErrUncertain) || key == "pending" && (err != nil || string(body) != `{"pending":true}`) {
							t.Fatalf("%s reservation lost: claimed=%t error=%v", key, claimed, err)
						}
					}
					changed := command
					changed.input = fmt.Sprintf("changed at %s", age)
					if claimed, _, err := s.claimHostedOperation(ctx, changed); claimed || !errors.Is(err, mutation.ErrConflict) {
						t.Fatalf("changed request escaped tombstone: claimed=%t error=%v", claimed, err)
					}
				}
				reconciled := command
				reconciled.key = "uncertain"
				if _, err := s.completeHostedOperation(ctx, reconciled, json.RawMessage(raw)); err != nil {
					t.Fatal(err)
				}
				if err := s.retainCommandPayloads(ctx, s.config.now()); err != nil {
					t.Fatal(err)
				}
				if claimed, body, err := s.claimHostedOperation(ctx, reconciled); claimed || err != nil || string(body) != raw {
					t.Fatalf("reconciliation did not restart replay horizon: claimed=%t error=%v", claimed, err)
				}
			})
		}
	}
}

func TestCommandPayloadConversionUsesBoundedBatches(t *testing.T) {
	now := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	f := newDefaultNativeFixture(t, Config{now: func() time.Time { return now }})
	s := f.service
	if err := s.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	if err := s.stopGitHubReconciliation(); err != nil {
		t.Fatal(err)
	}
	for _, family := range []struct {
		name  string
		count int
		until time.Time
	}{{"expired", 1, now.Add(-time.Second)}, {"inline", storageConversionRows + 1, now.Add(time.Hour)}} {
		if _, err := s.database.db.ExecContext(t.Context(), `WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM n WHERE i<?)
INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,created_at,response_pending,response_replay_until_ns)
SELECT ?,?,'example',?||i,'hash','{"output":"example"}',?,0,? FROM n`, family.count, f.project.OrganizationID, defaultHubOperatorID, family.name, formatHubTime(now), family.until.UnixNano()); err != nil {
			t.Fatal(err)
		}
	}
	for _, batch := range []struct {
		name            string
		expired, inline int
	}{{"first batch", 1, 1}, {"second batch", 1, 0}} {
		t.Run(batch.name, func(t *testing.T) {
			if err := s.retainCommandPayloads(t.Context(), now); err != nil {
				t.Fatal(err)
			}
			var expired, inline int
			if err := s.database.reader.QueryRowContext(t.Context(), `SELECT count(*) FILTER (WHERE response_expired=1),count(*) FILTER (WHERE command_key LIKE 'inline%' AND response_object_key='') FROM native_commands WHERE operation='example'`).Scan(&expired, &inline); err != nil {
				t.Fatal(err)
			}
			if expired != batch.expired || inline != batch.inline {
				t.Fatalf("expired=%d inline=%d, want %d and %d", expired, inline, batch.expired, batch.inline)
			}
		})
	}
}
