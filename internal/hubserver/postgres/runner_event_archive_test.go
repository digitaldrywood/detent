package postgres_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/postgres/pgstoretest"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/billingstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerReceiptRollsBackWhenArchiveDisappears(t *testing.T) {
	admin := pgstoretest.Template.Database(t)
	repository, _ := pgstoretest.RepositoryFor(t, admin, pgstoretest.SmallPool())
	if _, err := admin.ExecContext(t.Context(), `CREATE FUNCTION remove_unowned_receipt_archive() RETURNS trigger LANGUAGE plpgsql AS $$BEGIN DELETE FROM public.archive_objects a USING public.native_commands c WHERE a.organization_id=NEW.organization_id AND c.organization_id=a.organization_id AND c.command_key='archive-race' AND a.object_key=c.payload_object_key AND NOT EXISTS(SELECT 1 FROM public.archive_object_owners o WHERE o.organization_id=a.organization_id AND o.archive_id=a.id); RETURN NEW; END$$; CREATE TRIGGER remove_unowned_receipt_archive AFTER INSERT ON archive_objects FOR EACH ROW EXECUTE FUNCTION remove_unowned_receipt_archive()`); err != nil {
		t.Fatal(err)
	}
	content := eventstore.NewContent(pgstoretest.NewHistoryObjects())
	expectedBytes := make(map[tracker.OrganizationID]int64)
	for _, org := range []tracker.OrganizationID{"org_first", "org_second"} {
		pgstoretest.SeedLeaseFamily(t, repository, org)
		pgstoretest.SeedRunnerIdentity(t, repository, org, "runner_shared", "machine_1", pgstoretest.HistoryAt(), true)
		lease := pgstoretest.ClaimLease(t, repository, org, pgstoretest.LeaseClaim(1))
		receipt, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "runner_shared", Operation: "archive-race", CommandKey: "archive-race", RequestHash: strings.Repeat("a", 64), CreatedAt: pgstoretest.HistoryAt()}, json.RawMessage(`{"fixture":true}`))
		if err != nil {
			t.Fatal(err)
		}
		err = repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
			facts, ok := w.(interface {
				WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
			})
			if !ok {
				t.Fatal("runner command facts unavailable")
			}
			return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, ActorID: receipt.ActorID, Operation: receipt.Operation, Key: receipt.CommandKey}, func(w tenantstore.Writer) error { _, err := w.PutReceipt(ctx, receipt); return err })
		})
		if !errors.Is(err, corestore.ErrNotFound) {
			t.Fatalf("tenant=%s archive disappearance result=%v,want rejection", org, err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			_, err := r.Receipt(t.Context(), receipt.ActorID, receipt.Operation, receipt.CommandKey)
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("tenant=%s failed publication left a receipt: %v", org, err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		for _, mode := range []string{"point", "batch"} {
			if _, err := admin.ExecContext(t.Context(), `INSERT INTO hosted_usage_counters(organization_id,name,value) VALUES($1,'collaboration_bytes',0) ON CONFLICT (organization_id,name) DO NOTHING`, org); err != nil {
				t.Fatal(err)
			}
			var initial int64
			if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				initial, err = r.CollaborationBytes(t.Context())
				return err
			}); err != nil {
				t.Fatal(err)
			}
			charged, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "runner_shared", Operation: "archive-quota-" + mode, CommandKey: "archive-quota-" + mode, RequestHash: strings.Repeat("b", 64), CreatedAt: pgstoretest.HistoryAt()}, json.RawMessage(fmt.Sprintf(`{"quota":%q}`, mode)))
			if err != nil {
				t.Fatal(err)
			}
			ref := eventstore.Reference(charged.PayloadObjectKey, charged.PayloadSHA256, charged.PayloadBytes, charged.PayloadContentBytes)
			quotaRefused := errors.New("receipt archive allowance refused")
			for _, tc := range []struct {
				name   string
				limit  int64
				denied bool
			}{
				{name: "denied", limit: initial + ref.CompressedSize - 1, denied: true},
				{name: "accepted", limit: initial + ref.CompressedSize},
				{name: "replay", limit: initial + ref.CompressedSize},
			} {
				t.Run(string(org)+"/"+mode+"/"+tc.name, func(t *testing.T) {
					err := repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
						facts, ok := w.(interface {
							WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
						})
						if !ok {
							return errors.New("runner command facts unavailable")
						}
						return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, ActorID: charged.ActorID, Operation: charged.Operation, Key: charged.CommandKey}, func(w tenantstore.Writer) error {
							if err := w.LockArchiveAccounting(ctx); err != nil {
								return err
							}
							if _, err := w.PutReceipt(ctx, charged); err != nil {
								return err
							}
							var usage billingstore.UsageResult
							var err error
							if mode == "batch" {
								owner, ok := w.(interface {
									ChargeRunEventArchives(context.Context, []attachment.ArchiveReference, int64) (billingstore.UsageResult, error)
								})
								if !ok {
									return errors.New("runner archive batch owner unavailable")
								}
								usage, err = owner.ChargeRunEventArchives(ctx, []attachment.ArchiveReference{ref}, tc.limit)
							} else {
								object, readErr := w.ArchiveObjectByReference(ctx, ref)
								if readErr != nil {
									return readErr
								}
								usage, err = w.ChargeArchiveObject(ctx, object.ID, tc.limit)
							}
							if err != nil {
								return err
							}
							if !usage.Allowed {
								return quotaRefused
							}
							if usage.Used != initial+ref.CompressedSize {
								t.Fatalf("tenant=%s charged=%d want=%d", org, usage.Used, initial+ref.CompressedSize)
							}
							return nil
						})
					})
					if tc.denied && !errors.Is(err, quotaRefused) || !tc.denied && err != nil {
						t.Fatalf("tenant=%s quota publication=%v", org, err)
					}
					var acceptedReceipt eventstore.Receipt
					if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
						used, err := r.CollaborationBytes(t.Context())
						if err != nil {
							return err
						}
						want := initial + ref.CompressedSize
						if tc.denied {
							want = initial
						}
						if used != want {
							t.Fatalf("tenant=%s durable bytes=%d want=%d", org, used, want)
						}
						stored, receiptErr := r.Receipt(t.Context(), charged.ActorID, charged.Operation, charged.CommandKey)
						object, objectErr := r.ArchiveObjectByReference(t.Context(), ref)
						if tc.denied {
							if !errors.Is(receiptErr, corestore.ErrNotFound) || !errors.Is(objectErr, corestore.ErrNotFound) {
								t.Fatalf("tenant=%s refused charge left receipt=%v object=%v", org, receiptErr, objectErr)
							}
							return nil
						}
						if receiptErr != nil || objectErr != nil || stored.PayloadObjectKey != ref.Key || object.CollaborationBytes != ref.CompressedSize {
							t.Fatalf("tenant=%s receipt=%+v error=%v archive=%+v error=%v", org, stored, receiptErr, object, objectErr)
						}
						acceptedReceipt = stored
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					if !tc.denied {
						body, err := content.Receipt(t.Context(), acceptedReceipt)
						if err != nil || string(body) != fmt.Sprintf(`{"quota":%q}`, mode) {
							t.Fatalf("tenant=%s receipt body=%s error=%v", org, body, err)
						}
					}
				})
			}
			expectedBytes[org] = initial + ref.CompressedSize
			for tenant, expected := range expectedBytes {
				if err := repository.ReadTenant(t.Context(), tenant, corestore.Serving, func(r tenantstore.Reader) error {
					used, err := r.CollaborationBytes(t.Context())
					if err == nil && used != expected {
						t.Fatalf("tenant=%s another tenant changed bytes=%d want=%d", tenant, used, expected)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}
		}
		legacy, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: org, ActorID: "runner_shared", Operation: "archive-legacy", CommandKey: "archive-legacy", RequestHash: strings.Repeat("c", 64), CreatedAt: pgstoretest.HistoryAt()}, json.RawMessage(`{"quota":"legacy"}`))
		if err != nil {
			t.Fatal(err)
		}
		legacyRef := eventstore.Reference(legacy.PayloadObjectKey, legacy.PayloadSHA256, legacy.PayloadBytes, legacy.PayloadContentBytes)
		const legacyID = "archive_123456789abc4def8abc123456789abc"
		legacyUsed := expectedBytes[org] + legacyRef.CompressedSize
		if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockArchiveAccounting(t.Context()); err != nil {
				return err
			}
			if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: legacyID, Reference: legacyRef, CreatedAt: pgstoretest.HistoryAt()}); err != nil {
				return err
			}
			if err := w.AddArchiveOwner(t.Context(), artifactstore.ArchiveOwner{OrganizationID: org, ArchiveID: legacyID, Kind: "attempt-diff", ID: "example-legacy", CreatedAt: pgstoretest.HistoryAt()}); err != nil {
				return err
			}
			usage, err := w.AdjustCollaborationBytes(t.Context(), legacyRef.CompressedSize, math.MaxInt64)
			if err == nil && (!usage.Allowed || usage.Used != legacyUsed) {
				t.Fatalf("tenant=%s historical debit=%+v", org, usage)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
			facts, ok := w.(interface {
				WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
			})
			if !ok {
				return errors.New("runner command facts unavailable")
			}
			return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID, ActorID: legacy.ActorID, Operation: legacy.Operation, Key: legacy.CommandKey}, func(w tenantstore.Writer) error {
				if err := w.LockArchiveAccounting(ctx); err != nil {
					return err
				}
				if _, err := w.PutReceipt(ctx, legacy); err != nil {
					return err
				}
				owner, ok := w.(interface {
					ChargeRunEventArchives(context.Context, []attachment.ArchiveReference, int64) (billingstore.UsageResult, error)
				})
				if !ok {
					return errors.New("runner archive batch owner unavailable")
				}
				usage, err := owner.ChargeRunEventArchives(ctx, []attachment.ArchiveReference{legacyRef}, 0)
				if err == nil && (!usage.Allowed || usage.Used != legacyUsed) {
					t.Fatalf("tenant=%s legacy archive charged twice: %+v", org, usage)
				}
				return err
			})
		}); err != nil {
			t.Fatal(err)
		}
		expectedBytes[org] = legacyUsed
		for tenant, expected := range expectedBytes {
			if err := repository.ReadTenant(t.Context(), tenant, corestore.Serving, func(r tenantstore.Reader) error {
				used, err := r.CollaborationBytes(t.Context())
				if err != nil {
					return err
				}
				object, err := r.ArchiveObject(t.Context(), legacyID)
				if err != nil {
					return err
				}
				if used != expected || object.CollaborationBytes != object.Reference.CompressedSize {
					t.Fatalf("tenant=%s legacy recognition bytes=%d want=%d object=%+v", tenant, used, expected, object)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		lockOrder := errors.New("roll back queued inverse archive lock attempt")
		err = repository.WriteRunnerCommand(t.Context(), org, "runner_shared", "runner_shared", func(ctx context.Context, w tenantstore.Writer) error {
			facts, ok := w.(interface {
				WithRunnerEventFacts(context.Context, eventstore.RunCommand, func(tenantstore.Writer) error) error
			})
			if !ok {
				return errors.New("runner command facts unavailable")
			}
			return facts.WithRunnerEventFacts(ctx, eventstore.RunCommand{Project: "prj_1", WorkItem: "wi_1", LeaseID: lease.LeaseID}, func(w tenantstore.Writer) error {
				ref := eventstore.Reference(receipt.PayloadObjectKey, receipt.PayloadSHA256, receipt.PayloadBytes, receipt.PayloadContentBytes)
				if _, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_lock_order", Reference: ref, CreatedAt: receipt.CreatedAt}); err != nil {
					return err
				}
				if err := w.RemoveArchiveOwner(ctx, artifactstore.ArchiveOwner{OrganizationID: org, ArchiveID: "archive_lock_order", Kind: "native-command", ID: "lock_order"}); !errors.Is(err, corestore.ErrInvalidPage) {
					return errors.Join(err, errors.New("queued inverse final-owner refund accepted"))
				}
				if err := w.LockArchiveAccounting(ctx); !errors.Is(err, corestore.ErrInvalidPage) {
					return errors.Join(err, errors.New("queued inverse archive accounting lock accepted"))
				}
				return lockOrder
			})
		})
		if !errors.Is(err, lockOrder) {
			t.Fatalf("tenant=%s queued archive lock-order refusal=%v", org, err)
		}
		if err := repository.ReadTenant(t.Context(), org, corestore.Serving, func(r tenantstore.Reader) error {
			if _, err := r.ArchiveObject(t.Context(), "archive_lock_order"); !errors.Is(err, corestore.ErrNotFound) {
				return errors.Join(err, errors.New("queued archive survived lock-order refusal"))
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}
