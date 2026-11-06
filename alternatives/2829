package postgres_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestReceiptImportPreservesUncertainAndExpiredOutcomes(t *testing.T) {
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, smallPool())
	content := eventstore.NewContent(newHistoryObjects())
	observed := historyAt().Add(48 * time.Hour)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
		for _, tc := range []struct {
			name, operation                            string
			claimed, pending, expired, old, reconciled bool
		}{
			{name: "held", claimed: true, pending: true, old: true},
			{name: "async", pending: true, old: true},
			{name: "expired", expired: true},
			{name: "elapsed", old: true},
			{name: "ready"},
			{name: "historical", operation: "example.operation session_legacy"},
			{name: "historical-expired", operation: "example.operation session_legacy", expired: true},
			{name: "historical-held", operation: "example.operation session_legacy", claimed: true, pending: true, old: true},
			{name: "unreconciled", operation: "urgent_runner_update", old: true},
			{name: "reconciled", operation: "urgent_runner_update", old: true, reconciled: true},
		} {
			t.Run(string(org)+"/"+tc.name, func(t *testing.T) {
				operation := tc.operation
				if operation == "" {
					operation = "example.operation"
				}
				deadline := observed.Add(time.Hour)
				if tc.old {
					deadline = observed.Add(-time.Hour)
				}
				record := receiptstore.Record{OrganizationID: org, ActorID: "example-actor", Operation: operation, CommandKey: tc.name, RequestHash: artifact.Digest([]byte(tc.name)), CreatedAt: historyAt(), ResponseClaimed: tc.claimed, ResponsePending: tc.pending, ResponseExpired: tc.expired, ResponseReplayUntilNs: deadline.UnixNano()}
				if tc.pending && !tc.claimed {
					record.ResponseRequestID = "example-request"
				}
				if tc.reconciled {
					record.ResponseReconciliationHash = artifact.Digest([]byte("reconciled"))
				}
				wantExpired := tc.expired || tc.old && !tc.pending && (operation != "urgent_runner_update" || tc.reconciled)
				if !tc.claimed && !wantExpired {
					var err error
					record, err = content.PrepareReceipt(t.Context(), record, []byte(`{"result":"example"}`))
					if err != nil {
						t.Fatal(err)
					}
					record.ResponsePending = tc.pending
					record.ResponseRequestID = ""
					if tc.pending {
						record.ResponseRequestID = "example-request"
					}
				}
				source := receiptstore.Import{Record: record, SnapshotID: "example-snapshot", SourceHash: artifact.Digest([]byte(string(org) + tc.name)), ObservedAt: observed}
				var stored receiptstore.Record
				apply := func(source receiptstore.Import) error {
					return r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						var err error
						stored, err = w.(receiptstore.ImportWriter).ImportReceipt(t.Context(), source)
						return err
					})
				}
				if err := apply(source); err != nil {
					t.Fatal(err)
				}
				if stored.ResponseClaimed != tc.claimed || stored.ResponsePending != tc.pending || stored.ResponseExpired != wantExpired || stored.ResponseReplayUntilNs != deadline.UnixNano() || stored.ResponseReconciliationHash != record.ResponseReconciliationHash {
					t.Fatalf("import changed receipt state: claimed=%t pending=%t expired=%t", stored.ResponseClaimed, stored.ResponsePending, stored.ResponseExpired)
				}
				if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(reader tenantstore.Reader) error {
					replayed, err := reader.ReplayReceipt(t.Context(), record.ActorID, strings.SplitN(operation, " ", 2)[0], record.CommandKey)
					if err != nil {
						return err
					}
					if replayed.OrganizationID != org || replayed.Operation != operation || replayed.ResponseClaimed != tc.claimed || replayed.ResponseExpired != wantExpired {
						t.Fatalf("replay lost receipt identity or state: %+v", replayed)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if wantExpired {
					if _, err := content.Receipt(t.Context(), stored); !errors.Is(err, receiptstore.ErrExpired) {
						t.Fatalf("expired outcome read: %v", err)
					}
				}
				if err := apply(source); err != nil {
					t.Fatalf("exact source replay: %v", err)
				}
				if tc.claimed {
					completed, err := content.PrepareReceipt(t.Context(), receiptstore.Record{OrganizationID: org, ActorID: record.ActorID, Operation: operation, CommandKey: tc.name, RequestHash: record.RequestHash, CreatedAt: observed}, []byte(`{"complete":true}`))
					if err != nil {
						t.Fatal(err)
					}
					if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.(receiptstore.Writer).CompleteReceipt(t.Context(), completed)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					if err := apply(source); err != nil || stored.ResponseClaimed || stored.ResponsePending || stored.PayloadSHA256 != completed.PayloadSHA256 {
						t.Fatalf("source replay replaced newer completed outcome: %v", err)
					}
				}
				changed := source
				changed.SourceHash = artifact.Digest([]byte("different"))
				if err := apply(changed); !errors.Is(err, corestore.ErrRevisionConflict) {
					t.Fatalf("changed source replay: %v", err)
				}
			})
		}
	}
}

func TestReceiptMetadataBatchIsAtomicAndPreservesCompletion(t *testing.T) {
	admin := template.Database(t)
	pool := smallPool()
	pool.MaxConnections, pool.ServingConnections = 4, 2
	r, _ := repositoryFor(t, admin, pool)
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
		sources := make([]receiptstore.Import, 0, receiptstore.MaxImportBatch)
		for index := range receiptstore.MaxImportBatch {
			key := fmt.Sprintf("example-%04d", index)
			sources = append(sources, receiptstore.Import{Record: receiptstore.Record{OrganizationID: org, ActorID: "example-actor", Operation: "example.operation", CommandKey: key, RequestHash: artifact.Digest([]byte(key)), CreatedAt: historyAt(), ResponseClaimed: index == 0, ResponsePending: index == 0, ResponseExpired: index != 0, ResponseReplayUntilNs: historyAt().Add(receiptstore.ReplayRetention).UnixNano()}, SnapshotID: "example-snapshot", SourceHash: artifact.Digest([]byte(string(org) + key)), ObservedAt: historyAt()})
		}
		apply := func(page []receiptstore.Import) error {
			return r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				return w.(receiptstore.ImportWriter).ImportReceiptStates(t.Context(), page)
			})
		}
		for range 2 {
			if err := apply(sources); err != nil {
				t.Fatalf("batch apply/replay: %v", err)
			}
		}
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			return r.(receiptstore.ImportReader).VerifyReceiptStates(t.Context(), sources)
		}); err != nil {
			t.Fatal(err)
		}
		added := sources[1]
		added.Record.CommandKey = "example-added"
		added.SourceHash = artifact.Digest([]byte("added"))
		changed := sources[0]
		changed.SourceHash = artifact.Digest([]byte("changed"))
		if err := apply([]receiptstore.Import{added, changed}); !errors.Is(err, corestore.ErrRevisionConflict) {
			t.Fatalf("changed source batch: %v", err)
		}
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.Receipt(t.Context(), added.Record.ActorID, added.Record.Operation, added.Record.CommandKey)
			return err
		}); !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("failed batch retained partial insert: %v", err)
		}
		if err := apply(append(sources, added)); !errors.Is(err, corestore.ErrInvalidPage) {
			t.Fatalf("oversized metadata batch: %v", err)
		}
		complete, err := content.PrepareReceipt(t.Context(), sources[0].Record, []byte(`{"complete":true}`))
		if err != nil {
			t.Fatal(err)
		}
		complete.ResponseClaimed = false
		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		workers.Go(func() {
			<-start
			err := apply([]receiptstore.Import{sources[0], added})
			if errors.Is(err, receiptstore.ErrImmutableReceiptPresent) || errors.Is(err, corestore.ErrRevisionConflict) {
				err = nil
			}
			results <- err
		})
		workers.Go(func() {
			<-start
			results <- r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				_, err := w.(receiptstore.Writer).CompleteReceipt(t.Context(), complete)
				return err
			})
		})
		close(start)
		workers.Wait()
		for range 2 {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			actual, err := r.Receipt(t.Context(), complete.ActorID, complete.Operation, complete.CommandKey)
			if err == nil && (actual.ResponseClaimed || actual.ResponsePending || actual.ResponseExpired || actual.PayloadSHA256 != complete.PayloadSHA256) {
				t.Error("metadata replay replaced completed outcome")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := apply([]receiptstore.Import{sources[0]}); !errors.Is(err, receiptstore.ErrImmutableReceiptPresent) {
			t.Fatalf("body-bearing response entered metadata import: %v", err)
		}
	}
}

func TestReceiptMetadataBatchCannotMaskGenericMutation(t *testing.T) {
	admin := template.Database(t)
	pool := smallPool()
	pool.MaxConnections, pool.ServingConnections = 4, 2
	r, _ := repositoryFor(t, admin, pool)
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
		for _, order := range []string{"generic_first", "import_first", "concurrent"} {
			t.Run(string(org)+"/"+order, func(t *testing.T) {
				metadata := receiptstore.Import{Record: receiptstore.Record{OrganizationID: org, ActorID: "example-actor", Operation: "example.operation", CommandKey: order, RequestHash: artifact.Digest([]byte(order)), CreatedAt: historyAt(), ResponseExpired: true, ResponseReplayUntilNs: historyAt().Add(receiptstore.ReplayRetention).UnixNano()}, SnapshotID: "example-snapshot", SourceHash: artifact.Digest([]byte(string(org) + order)), ObservedAt: historyAt().Add(48 * time.Hour)}
				live := metadata.Record
				live.ResponseExpired = false
				live.ResponseReplayUntilNs = 0
				live, err := content.PrepareReceipt(t.Context(), live, []byte(`{"complete":true}`))
				if err != nil {
					t.Fatal(err)
				}
				project := corestore.Project{OrganizationID: org, ID: tracker.ProjectID("prj_" + order), Name: order, Slug: order, Profile: "native"}
				mutate := func() error {
					return r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						if _, err := w.CreateProject(t.Context(), project); err != nil {
							return err
						}
						_, err := w.PutReceipt(t.Context(), live)
						return err
					})
				}
				apply := func() error {
					return r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
						return w.(receiptstore.ImportWriter).ImportReceiptStates(t.Context(), []receiptstore.Import{metadata})
					})
				}
				var mutationErr, importErr error
				switch order {
				case "generic_first":
					mutationErr, importErr = mutate(), apply()
				case "import_first":
					importErr, mutationErr = apply(), mutate()
				default:
					start := make(chan struct{})
					var workers sync.WaitGroup
					workers.Go(func() { <-start; mutationErr = mutate() })
					workers.Go(func() { <-start; importErr = apply() })
					close(start)
					workers.Wait()
				}
				if mutationErr == nil {
					if !errors.Is(importErr, receiptstore.ErrImmutableReceiptPresent) && !errors.Is(importErr, corestore.ErrRevisionConflict) {
						t.Fatalf("live mutation lost import fence: %v", importErr)
					}
				} else if !errors.Is(mutationErr, corestore.ErrRevisionConflict) || importErr != nil {
					t.Fatalf("metadata mutation=%v import=%v", mutationErr, importErr)
				}
				if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					stored, err := r.Receipt(t.Context(), live.ActorID, live.Operation, live.CommandKey)
					if err != nil {
						return err
					}
					_, projectErr := r.Project(t.Context(), project.ID)
					if mutationErr == nil {
						if stored.ResponseReplayUntilNs != 0 {
							t.Error("generic receipt acquired hosted replay expiry")
						}
						if stored.ResponseExpired || stored.PayloadSHA256 != live.PayloadSHA256 || projectErr != nil {
							t.Errorf("metadata masked committed mutation: expired=%t project=%v", stored.ResponseExpired, projectErr)
						}
					} else if !stored.ResponseExpired || !errors.Is(projectErr, corestore.ErrNotFound) {
						t.Errorf("rejected mutation retained partial resource: expired=%t project=%v", stored.ResponseExpired, projectErr)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReceiptStateRollbackRefusesAcrossTenantRLS(t *testing.T) {
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, smallPool())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			_, _, err := w.(receiptstore.Writer).ClaimReceipt(t.Context(), receiptstore.Record{OrganizationID: org, ActorID: "example-actor", Operation: "example.operation", CommandKey: "example-key", RequestHash: artifact.Digest([]byte("request")), CreatedAt: historyAt(), ResponseClaimed: true, ResponsePending: true})
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	admin.SetMaxIdleConns(1)
	if _, err := admin.ExecContext(t.Context(), `SET ROLE detent_migration_v2`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if _, err := admin.ExecContext(t.Context(), `RESET ROLE`); err != nil {
			t.Error(err)
		}
	}()
	var hidden int
	if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM native_command_states`).Scan(&hidden); err != nil || hidden != 0 {
		t.Fatalf("unscoped migration role did not exercise tenant RLS: count=%d error=%v", hidden, err)
	}
	provider, err := migrationProvider(admin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.ApplyVersion(t.Context(), 20261103193325, false); err == nil || !strings.Contains(err.Error(), "explicit retained-data migration") {
		t.Fatalf("rollback returned %v, want retained-data refusal", err)
	}
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			stored, err := r.Receipt(t.Context(), "example-actor", "example.operation", "example-key")
			if err == nil && !stored.ResponseClaimed {
				t.Error("rollback changed retained command claim")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReceiptImportCannotReplacePreStateCompletion(t *testing.T) {
	admin := template.Database(t)
	r, _ := repositoryFor(t, admin, smallPool())
	content := eventstore.NewContent(newHistoryObjects())
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, historyAt()); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"claimed", "expired", "completed"} {
			t.Run(string(org)+"/"+mode, func(t *testing.T) {
				live, err := content.PrepareReceipt(t.Context(), receiptstore.Record{OrganizationID: org, ActorID: "example-actor", Operation: "example.operation", CommandKey: mode, RequestHash: artifact.Digest([]byte(mode)), CreatedAt: historyAt()}, []byte(`{"complete":true}`))
				if err != nil {
					t.Fatal(err)
				}
				if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.PutReceipt(t.Context(), live)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				if _, err := admin.ExecContext(t.Context(), `DELETE FROM native_command_states WHERE organization_id=$1 AND actor_id=$2 AND operation=$3 AND command_key=$4`, org, live.ActorID, live.Operation, mode); err != nil {
					t.Fatal(err)
				}
				var before int64
				if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					var err error
					before, err = r.ArchiveBytes(t.Context())
					return err
				}); err != nil {
					t.Fatal(err)
				}
				source := receiptstore.Import{Record: live, SnapshotID: "example-snapshot", SourceHash: artifact.Digest([]byte("source")), ObservedAt: historyAt()}
				if mode != "completed" {
					source.Record.PayloadObjectKey, source.Record.PayloadSHA256, source.Record.PayloadBytes, source.Record.PayloadContentBytes = "", "", 0, 0
					source.Record.ResponseClaimed = mode == "claimed"
					source.Record.ResponsePending = mode == "claimed"
					source.Record.ResponseExpired = mode == "expired"
				}
				if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
					_, err := w.(receiptstore.ImportWriter).ImportReceipt(t.Context(), source)
					return err
				}); !errors.Is(err, receiptstore.ErrImmutableReceiptPresent) {
					t.Fatalf("pre-state completed response import returned %v", err)
				}
				if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
					stored, err := r.Receipt(t.Context(), live.ActorID, live.Operation, live.CommandKey)
					if err != nil {
						return err
					}
					if stored.ResponseClaimed || stored.ResponsePending || stored.ResponseExpired || stored.PayloadSHA256 != live.PayloadSHA256 {
						t.Error("source import replaced completed response visibility")
					}
					after, err := r.ArchiveBytes(t.Context())
					if err == nil && after != before {
						t.Errorf("source import changed archive charge: before=%d after=%d", before, after)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestReceiptClaimHasOneOwnerAndExpiryReleasesPayload(t *testing.T) {
	admin := template.Database(t)
	pool := smallPool()
	pool.MaxConnections, pool.ServingConnections = 4, 2
	r, _ := repositoryFor(t, admin, pool)
	content := eventstore.NewContent(newHistoryObjects())
	org := tracker.OrganizationID("org_first")
	if _, err := admin.ExecContext(t.Context(), `INSERT INTO organizations(organization_id,name,local,created_at) VALUES($1,$1,false,$2)`, org, historyAt()); err != nil {
		t.Fatal(err)
	}
	claim := receiptstore.Record{OrganizationID: org, ActorID: "example-actor", Operation: "example.operation", CommandKey: "example-key", RequestHash: artifact.Digest([]byte("request")), CreatedAt: historyAt(), ResponseClaimed: true, ResponsePending: true}
	start := make(chan struct{})
	results := make(chan bool, 2)
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() {
			<-start
			var owned bool
			err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				var err error
				_, owned, err = w.(receiptstore.Writer).ClaimReceipt(t.Context(), claim)
				return err
			})
			results <- owned
			errors <- err
		})
	}
	close(start)
	workers.Wait()
	owners := 0
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
		if <-results {
			owners++
		}
	}
	if owners != 1 {
		t.Fatalf("concurrent claim owners=%d, want one", owners)
	}
	complete, err := content.PrepareReceipt(t.Context(), receiptstore.Record{OrganizationID: org, ActorID: claim.ActorID, Operation: claim.Operation, CommandKey: claim.CommandKey, RequestHash: claim.RequestHash, CreatedAt: historyAt()}, []byte(`{"complete":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.(receiptstore.Writer).CompleteReceipt(t.Context(), complete)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cutoff := historyAt().Add(receiptstore.ReplayRetention + time.Second)
	var page []receiptstore.Record
	var charged int64
	if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		page, err = r.(receiptstore.RetentionReader).ReceiptExpiryPage(t.Context(), cutoff, 1)
		if err != nil {
			return err
		}
		charged, err = r.ArchiveBytes(t.Context())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(page) != 1 || charged != complete.PayloadBytes {
		t.Fatalf("expiry page=%d charged=%d payload=%d", len(page), charged, complete.PayloadBytes)
	}
	if err := r.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
		changed, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), page[0], cutoff)
		if err == nil && !changed {
			t.Error("eligible response did not expire")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := r.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
		stored, err := r.Receipt(t.Context(), claim.ActorID, claim.Operation, claim.CommandKey)
		if err != nil {
			return err
		}
		if !stored.ResponseExpired || stored.PayloadSHA256 != complete.PayloadSHA256 {
			t.Error("expiry removed immutable receipt identity")
		}
		charged, err = r.ArchiveBytes(t.Context())
		return err
	}); err != nil || charged != 0 {
		t.Fatalf("expired archive charge=%d error=%v", charged, err)
	}
}
