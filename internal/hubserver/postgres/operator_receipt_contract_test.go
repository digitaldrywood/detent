package postgres_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOperatorReceiptGenericWritesCannotBypassCustody(t *testing.T) {
	admin := template.Database(t)
	repository, _ := repositoryFor(t, admin, smallPool())
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{"complete", "abandon"} {
			claim := receiptstore.Record{OrganizationID: org, ActorID: "example-actor", Operation: "example.operation", CommandKey: key, RequestHash: artifact.Digest([]byte(key)), CreatedAt: historyAt(), ResponseClaimed: true, ResponsePending: true}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, _, err := w.(receiptstore.Writer).ClaimReceipt(t.Context(), claim)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := admin.ExecContext(t.Context(), `UPDATE native_command_states SET reservation_project_id='prj_example',reservation_correlation_id='example-correlation' WHERE organization_id=$1 AND command_key=$2`, org, key); err != nil {
				t.Fatal(err)
			}
			completed, err := content.PrepareReceipt(t.Context(), receiptstore.Record{OrganizationID: org, ActorID: claim.ActorID, Operation: claim.Operation, CommandKey: key, RequestHash: claim.RequestHash, CreatedAt: historyAt()}, []byte(`{"outcome":"succeeded"}`))
			if err != nil {
				t.Fatal(err)
			}
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if key == "abandon" {
					return w.(receiptstore.Writer).AbandonReceipt(t.Context(), claim.ActorID, claim.Operation, key, claim.RequestHash)
				}
				_, err := w.(receiptstore.Writer).CompleteReceipt(t.Context(), completed)
				return err
			})
			if !errors.Is(err, mutation.ErrUncertain) {
				t.Errorf("%s/%s generic custody bypass error = %v, want uncertain", org, key, err)
			}
		}
	}
}

func TestOperatorReceiptFencesProjectLifetime(t *testing.T) {
	admin := template.Database(t)
	config := smallPool()
	config.MaxConnections, config.ServingConnections = 4, 2
	repository, pool := repositoryFor(t, admin, config)
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: "Example organization", CreatedAt: historyAt()}); err != nil {
				return err
			}
			for _, suffix := range []string{"first", "second"} {
				_, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: org, ID: tracker.ProjectID("prj_" + suffix), Name: suffix, Slug: suffix, Profile: "native"}, States: []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true, Transitions: []string{"Todo"}}}})
				if err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO native_command_states(organization_id,actor_id,operation,command_key,request_hash,created_at,response_expired) SELECT $1,'example-history','example.history',n::text,repeat('a',64),$2,true FROM generate_series(1,10000) n`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
		global := postgres.OperatorReceiptReservation{Receipt: receiptstore.Record{OrganizationID: org, ActorID: "example-principal", Operation: "example.command", CommandKey: "organization", RequestHash: artifact.Digest([]byte("organization")), CreatedAt: historyAt()}, CorrelationID: "example-correlation"}
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, _, err := w.(postgres.OperatorReceiptWriter).ReserveOperatorReceipt(t.Context(), global)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		for _, project := range []tracker.ProjectID{"prj_first", "prj_second"} {
			request := global
			request.ProjectID, request.Receipt.CommandKey = project, string(project)
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.RequireProjectIdle(t.Context(), project, historyAt()); err != nil {
					return err
				}
				_, _, err := w.(postgres.OperatorReceiptWriter).ReserveOperatorReceipt(t.Context(), request)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if project == "prj_second" {
				if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, claimed, err := w.(postgres.OperatorReceiptWriter).ClaimOperatorReceipt(t.Context(), request)
					if err == nil && !claimed {
						t.Error("reserved execution was not claimed")
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error { return w.RequireProjectIdle(t.Context(), project, historyAt()) }); !errors.Is(err, projectstore.ErrActiveWork) {
				t.Errorf("pending operator custody permits idle project: %v", err)
			}
			assertOperatorProjectActivitySeek(t, pool, org, project)
			rollback := errors.New("unexpected retirement rolled back")
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.RetireProject(t.Context(), project, 1, historyAt()); err != nil {
					return err
				}
				return rollback
			}); !errors.Is(err, projectstore.ErrActiveWork) {
				t.Errorf("pending operator custody permits retirement: %v", err)
			}
			response, err := content.PrepareReceipt(t.Context(), request.Receipt, []byte(`{"outcome":"rejected"}`))
			if err != nil {
				t.Fatal(err)
			}
			completion := request
			completion.Receipt = response
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.(postgres.OperatorReceiptWriter).CompleteOperatorReceipt(t.Context(), completion, true); err != nil {
					return err
				}
				return w.RequireProjectIdle(t.Context(), project, historyAt())
			}); err != nil {
				t.Fatal(err)
			}
			locked, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			locker := startRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.LockWorkItemProject(ctx, project); err != nil {
						return err
					}
					close(locked)
					select {
					case <-release:
						return w.RetireProject(ctx, project, 1, historyAt())
					case <-ctx.Done():
						return ctx.Err()
					}
				})
			})
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			waitRepositorySignal(t, locked)
			var lockerPID int
			if err := admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND state='idle in transaction' AND query LIKE '-- name: LockWorkItemProject :one%'`, pool.Config().ConnConfig.User).Scan(&lockerPID); err != nil {
				t.Fatal(err)
			}
			race := request
			race.Receipt.CommandKey += "-after-retirement"
			reserver := startRepositoryTask(t, func(ctx context.Context) error {
				return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					_, _, err := w.(postgres.OperatorReceiptWriter).ReserveOperatorReceipt(ctx, race)
					return err
				})
			})
			t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
			testenv.WaitUntil(t, t.Context(), func() error {
				select {
				case <-reserver.done:
					t.Fatal("reservation completed without waiting for the project retirement lock")
				default:
				}
				var pid int
				return admin.QueryRowContext(t.Context(), `SELECT pid FROM pg_stat_activity WHERE datname=current_database() AND usename=$1 AND wait_event_type='Lock' AND $2=ANY(pg_blocking_pids(pid)) AND query LIKE '-- name: LockWorkItemProject :one%'`, pool.Config().ConnConfig.User, lockerPID).Scan(&pid)
			})
			releaseOnce.Do(func() { close(release) })
			if err := locker.wait(t); err != nil {
				t.Fatal(err)
			}
			if err := reserver.wait(t); !errors.Is(err, corestore.ErrNotFound) && !errors.Is(err, corestore.ErrSerializationFailure) {
				t.Fatalf("reservation accepted retired project: %v", err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				_, err := r.(postgres.OperatorReceiptReader).OperatorReceipt(t.Context(), project, race.Receipt)
				return err
			}); !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("retirement race persisted a reservation: %v", err)
			}
		}
	}
}

func assertOperatorProjectActivitySeek(t *testing.T, pool *pgxpool.Pool, organization tracker.OrganizationID, project tracker.ProjectID) {
	t.Helper()
	tx, err := pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := tx.Rollback(testenv.WaitContext(t, context.WithoutCancel(t.Context()))); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			t.Error(err)
		}
	})
	if _, err := tx.Exec(t.Context(), `SELECT set_config('detent.organization_id', $1, true)`, string(organization)); err != nil {
		t.Fatal(err)
	}
	var raw []byte
	if err := tx.QueryRow(t.Context(), `EXPLAIN (ANALYZE,FORMAT JSON) SELECT EXISTS(SELECT 1 FROM native_command_states WHERE organization_id=$1 AND reservation_project_id=$2 AND reservation_correlation_id IS NOT NULL AND response_claimed)`, organization, project).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	type plan struct {
		Index string  `json:"Index Name"`
		Cond  string  `json:"Index Cond"`
		Rows  float64 `json:"Actual Rows"`
		Plans []plan
	}
	var plans []struct{ Plan plan }
	if err := json.Unmarshal(raw, &plans); err != nil {
		t.Fatal(err)
	}
	found := false
	var visit func(plan)
	visit = func(node plan) {
		if node.Index == "native_command_operator_project_active" {
			found = true
			if node.Rows > 1 || !strings.Contains(node.Cond, "organization_id") || !strings.Contains(node.Cond, "reservation_project_id") {
				t.Errorf("operator activity did not seek one scoped row: rows=%v condition=%s", node.Rows, node.Cond)
			}
		}
		for _, child := range node.Plans {
			visit(child)
		}
	}
	for _, root := range plans {
		visit(root.Plan)
	}
	if !found {
		t.Fatal("operator project activity did not use its partial index")
	}
	if err := tx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestOperatorReceiptPreservesScopedCustodyAndReplay(t *testing.T) {
	admin := template.Database(t)
	seedCore(t, admin)
	config := smallPool()
	config.MaxConnections, config.ServingConnections = 4, 2
	repository, _ := repositoryFor(t, admin, config)
	content := eventstore.NewContent(newHistoryObjects())
	var completed []postgres.OperatorReceiptReservation
	var payloads [][]byte
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		for _, project := range []tracker.ProjectID{"prj_shared", "prj_other"} {
			identity := receiptstore.Record{OrganizationID: org, ActorID: "example-principal", Operation: "example.command", CommandKey: artifact.Digest([]byte(project)), RequestHash: artifact.Digest([]byte("request:" + project)), CreatedAt: historyAt()}
			reservation := postgres.OperatorReceiptReservation{Receipt: identity, ProjectID: project}
			start := make(chan struct{})
			var tasks [2]*repositoryTask
			var inserted [2]bool
			var reserved [2]postgres.OperatorReceiptState
			for index := range tasks {
				candidate := reservation
				candidate.CorrelationID = fmt.Sprintf("example-correlation-%d", index)
				tasks[index] = startRepositoryTask(t, func(ctx context.Context) error {
					select {
					case <-start:
					case <-ctx.Done():
						return ctx.Err()
					}
					return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						var err error
						reserved[index], inserted[index], err = w.(postgres.OperatorReceiptWriter).ReserveOperatorReceipt(ctx, candidate)
						return err
					})
				})
			}
			close(start)
			owners := 0
			for index, task := range tasks {
				if err := task.wait(t); err != nil {
					if errors.Is(err, corestore.ErrSerializationFailure) {
						continue
					}
					t.Fatal(err)
				}
				if inserted[index] {
					owners++
					reservation.CorrelationID = reserved[index].CorrelationID
					if reserved[index].ClaimedAt != nil || !reserved[index].Receipt.ResponseClaimed || !reserved[index].Receipt.ResponsePending {
						t.Fatal("reservation already authorized execution")
					}
				}
			}
			if owners != 1 {
				t.Fatalf("reservation owners = %d, want one", owners)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(postgres.OperatorReceiptWriter)
				changed := reservation
				changed.CorrelationID = "different-correlation"
				state, owned, err := owner.ReserveOperatorReceipt(t.Context(), changed)
				if err != nil || owned || state.CorrelationID != reservation.CorrelationID {
					return fmt.Errorf("reservation takeover: owned=%t error=%w", owned, err)
				}
				_, owned, err = owner.ClaimOperatorReceipt(t.Context(), changed)
				if err != nil || owned {
					return fmt.Errorf("foreign correlation claimed: owned=%t error=%w", owned, err)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name     string
				project  tracker.ProjectID
				expected receiptstore.Record
				want     error
			}{
				{"project", "prj_wrong", identity, corestore.ErrNotFound},
				{"principal", project, func() receiptstore.Record { v := identity; v.ActorID = "another-principal"; return v }(), corestore.ErrNotFound},
				{"hash", project, func() receiptstore.Record {
					v := identity
					v.RequestHash = artifact.Digest([]byte("changed"))
					return v
				}(), mutation.ErrConflict},
				{"tenant", project, func() receiptstore.Record { v := identity; v.OrganizationID = "org_wrong"; return v }(), corestore.ErrTenantMismatch},
			} {
				err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					_, err := r.(postgres.OperatorReceiptReader).OperatorReceipt(t.Context(), test.project, test.expected)
					return err
				})
				if !errors.Is(err, test.want) {
					t.Fatalf("%s read error = %v, want %v", test.name, err, test.want)
				}
				candidate := reservation
				candidate.ProjectID, candidate.Receipt = test.project, test.expected
				err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, _, err := w.(postgres.OperatorReceiptWriter).ClaimOperatorReceipt(t.Context(), candidate)
					return err
				})
				if !errors.Is(err, test.want) {
					t.Fatalf("%s claim error = %v, want %v", test.name, err, test.want)
				}
			}
			configuration := []byte("{\n  \"project\": \"" + string(project) + "\", \"value\": 7\n}\n")
			outcome := "succeeded"
			rejected := project == "prj_other"
			if rejected {
				outcome = "rejected"
			}
			payload, err := json.Marshal(struct {
				Outcome       string `json:"outcome"`
				Configuration []byte `json:"configuration_receipt_bytes"`
			}{outcome, configuration})
			if err != nil {
				t.Fatal(err)
			}
			response, err := content.PrepareReceipt(t.Context(), identity, payload)
			if err != nil {
				t.Fatal(err)
			}
			completion := reservation
			completion.Receipt = response
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.(postgres.OperatorReceiptWriter).CompleteOperatorReceipt(t.Context(), completion, false)
				return err
			})
			if !errors.Is(err, mutation.ErrUncertain) {
				t.Fatalf("unclaimed success completion = %v, want uncertain", err)
			}
			if !rejected {
				start = make(chan struct{})
				var claimed [2]bool
				for index := range tasks {
					tasks[index] = startRepositoryTask(t, func(ctx context.Context) error {
						select {
						case <-start:
						case <-ctx.Done():
							return ctx.Err()
						}
						return repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
							_, owned, err := w.(postgres.OperatorReceiptWriter).ClaimOperatorReceipt(ctx, reservation)
							claimed[index] = owned
							return err
						})
					})
				}
				close(start)
				owners = 0
				for index, task := range tasks {
					if err := task.wait(t); err != nil {
						if errors.Is(err, corestore.ErrSerializationFailure) {
							continue
						}
						t.Fatal(err)
					}
					if claimed[index] {
						owners++
					}
				}
				if owners != 1 {
					t.Fatalf("execution owners = %d, want one", owners)
				}
			}
			rollback := errors.New("injected business write failure")
			err = repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if _, err := w.(postgres.OperatorReceiptWriter).CompleteOperatorReceipt(t.Context(), completion, rejected); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatal(err)
			}
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				state, err := r.(postgres.OperatorReceiptReader).OperatorReceipt(t.Context(), project, identity)
				if err == nil && (!state.Receipt.ResponseClaimed || state.Receipt.PayloadObjectKey != "") {
					t.Error("rolled-back completion released custody or published a body")
				}
				_, bodyErr := content.Receipt(t.Context(), state.Receipt)
				if !errors.Is(bodyErr, mutation.ErrUncertain) {
					t.Errorf("pending response = %v, want uncertain", bodyErr)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				owner := w.(postgres.OperatorReceiptWriter)
				foreign := completion
				foreign.CorrelationID = "different-correlation"
				if _, err := owner.CompleteOperatorReceipt(t.Context(), foreign, rejected); !errors.Is(err, mutation.ErrUncertain) {
					return fmt.Errorf("foreign completion: %w", err)
				}
				_, err := owner.CompleteOperatorReceipt(t.Context(), completion, rejected)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			completed = append(completed, completion)
			payloads = append(payloads, configuration)
		}
	}
	if err := repository.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := repositoryFor(t, admin, config)
	for index, request := range completed {
		var state postgres.OperatorReceiptState
		if err := reopened.ReadTenant(t.Context(), request.Receipt.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			state, err = r.(postgres.OperatorReceiptReader).OperatorReceipt(t.Context(), request.ProjectID, request.Receipt)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		raw, err := content.Receipt(t.Context(), state.Receipt)
		if err != nil {
			t.Fatal(err)
		}
		var value struct {
			Configuration []byte `json:"configuration_receipt_bytes"`
		}
		if err := json.Unmarshal(raw, &value); err != nil || !bytes.Equal(value.Configuration, payloads[index]) || state.Receipt.ResponseReplayUntilNs != 0 {
			t.Fatalf("reopened response changed: error=%v deadline=%d", err, state.Receipt.ResponseReplayUntilNs)
		}
		if err := reopened.WriteTenant(t.Context(), request.Receipt.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			owner := w.(postgres.OperatorReceiptWriter)
			if _, claimed, err := owner.ClaimOperatorReceipt(t.Context(), request); err != nil || claimed {
				return fmt.Errorf("completed command reexecuted: claimed=%t error=%w", claimed, err)
			}
			prior, err := owner.CompleteOperatorReceipt(t.Context(), request, request.ProjectID == "prj_other")
			if err == nil && prior.Receipt.PayloadSHA256 != state.Receipt.PayloadSHA256 {
				t.Error("completion replay replaced the canonical response")
			}
			if err != nil {
				return err
			}
			expired, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), prior.Receipt, historyAt().Add(48*time.Hour))
			if err == nil && expired {
				t.Error("operator response lost durable replay")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}
