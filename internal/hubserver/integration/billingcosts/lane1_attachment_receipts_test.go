package billingcosts_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/changefiles"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubcontent"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestAttachmentReceiptSharedContentBilling(t *testing.T) {
	t.Parallel()
	repository := hubtest.Repository(t)
	for caseIndex, test := range []struct {
		name          string
		billable      []bool
		allowance     int64
		completion    bool
		customerOwned bool
		detailStore   bool
		wantLimit     string
	}{
		{name: "receipt first", billable: []bool{false, true, false}, allowance: 10000},
		{name: "content first", billable: []bool{true, false, true}, allowance: 10000},
		{name: "receipt only", billable: []bool{false, false}},
		{name: "detail store shared", billable: []bool{false, true, false}, allowance: 10000, detailStore: true},
		{name: "API refusal", billable: []bool{true}, allowance: 10000, wantLimit: "api_mutations"},
		{name: "quota refusal", billable: []bool{true}, wantLimit: "collaboration_bytes"},
		{name: "completion while exhausted", billable: []bool{true, false, true}, completion: true},
		{name: "customer owned", billable: []bool{false, true, false}, customerOwned: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			plans := hostedplans.PilotPlans()
			for i := range plans.Plans {
				plans.Plans[i].Allowances["collaboration_bytes"] = test.allowance
				plans.Plans[i].Allowances["api_mutations"] = 10000
				if test.wantLimit == "api_mutations" || test.completion {
					plans.Plans[i].Allowances["api_mutations"] = 0
				}
			}
			for tenant := range 2 {
				organization := tracker.OrganizationID(fmt.Sprintf("org_receipt_%d_%d", caseIndex, tenant))
				t.Run(string(organization), func(t *testing.T) {
					hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: organization, Name: string(organization)})
					billing := hostedplans.New(hostedplans.Deps{Repository: repository, Details: hubtest.NewDetails(), Organization: organization})
					config := &hostedplans.HostedConfig{OrganizationID: string(organization), Plans: &plans, EntitlementAdministrator: "operator"}
					if err := hubapi.FromFeatureError(billing.ConfigurePlans(t.Context(), hostedplans.PlansInput(config))); err != nil {
						t.Fatal(err)
					}
					service := changefiles.NewContent(changefiles.ContentDeps{Repository: repository, Clock: time.Now, Plans: billing.Plans, EntitlementRecord: billing.EntitlementRecord})
					var stored []byte
					owner := &hubcontent.ChangeContent{Register: service.RegisterChangeContent, Completion: test.completion}
					if test.detailStore {
						owner.Store = hubtest.NewDetails()
					} else {
						owner.Call = func(_ context.Context, request entryauthority.ContentRequest) ([]byte, error) {
							if request.Write {
								stored = bytes.Clone(request.Data)
								return nil, nil
							}
							return bytes.Clone(stored), nil
						}
					}
					ctx := context.WithValue(t.Context(), hubcontent.ChangeContentKey{}, owner)
					var charged bool
					var previous attachment.ArchiveReference
					for _, billable := range test.billable {
						var ref attachment.ArchiveReference
						var err error
						if test.customerOwned {
							ref, _, err = attachment.PrepareArchive(string(organization), 1, strings.NewReader("shared attachment metadata"))
							if err != nil {
								t.Fatal(err)
							}
							ref.CustomerOwned, ref.BindingID = true, "binding_example"
							err = service.RegisterChangeContent(ctx, string(organization), ref, billable)
						} else {
							ref, err = changefiles.PrepareMeteredContent(ctx, string(organization), []byte("shared attachment metadata"), billable)
						}
						var limit *hostedplans.LimitError
						if test.wantLimit != "" {
							if !errors.As(err, &limit) || limit.Resource != test.wantLimit {
								t.Fatalf("billable=%t limit=%v err=%v", billable, limit, err)
							}
						} else if err != nil {
							t.Fatal(err)
						}
						if previous.Key != "" && previous != ref {
							t.Fatalf("shared reference changed: before=%+v after=%+v", previous, ref)
						}
						previous = ref
						charged = charged || billable && !test.customerOwned && test.wantLimit == ""
						var want int64
						if charged {
							want = ref.CompressedSize
						}
						if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							used, err := r.CollaborationBytes(t.Context())
							if err != nil {
								return err
							}
							if used != want {
								t.Fatalf("billable=%t usage=%d want=%d", billable, used, want)
							}
							object, err := r.ArchiveObjectByReference(t.Context(), ref)
							if test.wantLimit != "" {
								if !errors.Is(err, corestore.ErrNotFound) {
									t.Fatalf("refused registration persisted: object=%+v err=%v", object, err)
								}
								return nil
							}
							if err == nil && (object.OrganizationID != organization || object.CollaborationBytes != want || object.Reference != ref) {
								t.Fatalf("registered object=%+v want debit=%d", object, want)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					if test.wantLimit != "" {
						if len(stored) != 0 {
							t.Fatal("quota refusal retained object bytes")
						}
						return
					}
					if err := service.RegisterChangeContent(ctx, string(organization), previous, charged || test.customerOwned); err != nil {
						t.Fatalf("durable registration replay: %v", err)
					}
					if err := service.RegisterChangeContent(t.Context(), string(organization)+"_foreign", previous, true); !errors.Is(err, attachment.ErrInvalid) {
						t.Fatalf("foreign organization registration=%v", err)
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						used, err := r.CollaborationBytes(t.Context())
						want := int64(0)
						if charged {
							want = previous.CompressedSize
						}
						if err != nil {
							return err
						}
						if used != want {
							t.Fatalf("durable replay usage=%d want=%d", used, want)
						}
						now := time.Now()
						window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
						apiUsed, err := r.BillingUsage(t.Context(), window, "api_mutations")
						if err != nil && !errors.Is(err, corestore.ErrNotFound) {
							return err
						}
						if apiUsed != 0 {
							t.Fatalf("registration replay charged API mutations=%d", apiUsed)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestAttachmentReceiptBillingMigration(t *testing.T) {
	t.Parallel()
	objects := hubtest.NewDetails()
	h := hubfixture.Open(t, hubfixture.Config{RunDetails: objects})
	parent := t
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			ctx := t.Context()
			repository := h.Repository
			content := eventstore.NewContent(objects)
			var initial int64
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				initial, err = r.CollaborationBytes(ctx)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var total int64
			var shared attachment.ArchiveReference
			var receipts []eventstore.Receipt
			for _, operation := range []string{"attachment.record example", "example.record", "attachment.record shared"} {
				raw, err := json.Marshal(map[string]string{"operation": operation})
				if err != nil {
					t.Fatal(err)
				}
				ref, err := objects.StoreDetail(ctx, string(org), raw)
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: org, ActorID: "historical-operator", Operation: operation, CommandKey: operation, RequestHash: artifact.Digest([]byte(operation)), CreatedAt: h.Now()}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if receipt.PayloadObjectKey != ref.Key {
					t.Fatal("command receipt did not share its registered content")
				}
				if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.LockArchiveAccounting(ctx); err != nil {
						return err
					}
					object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: receipt.CreatedAt})
					if err != nil {
						return err
					}
					charged, err := w.ChargeArchiveObject(ctx, object.ID, math.MaxInt64)
					if err != nil {
						return err
					}
					if !charged.Allowed {
						return errors.New("historical fixture archive debit refused")
					}
					_, err = w.PutReceipt(ctx, receipt)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				total += ref.CompressedSize
				receipts = append(receipts, receipt)
				if operation == "attachment.record shared" {
					shared = ref
					receipt.Operation, receipt.CommandKey = "example.shared", "shared"
					if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.PutReceipt(ctx, receipt)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					receipts = append(receipts, receipt)
				}
			}
			for _, stage := range []string{"registered history", "durable replay", "restart replay"} {
				if stage == "restart replay" {
					if err := h.Service.Close(); err != nil {
						t.Fatal(err)
					}
					h = h.Reopen(parent)
					repository = h.Repository
				}
				if stage != "registered history" {
					service := changefiles.NewContent(changefiles.ContentDeps{
						Repository: repository, Clock: h.Config.Clock,
						Plans: func() *hostedplans.HostedPlansConfig { return nil },
					})
					for _, receipt := range receipts {
						ref := eventstore.Reference(receipt.PayloadObjectKey, receipt.PayloadSHA256, receipt.PayloadBytes, receipt.PayloadContentBytes)
						if err := service.RegisterChangeContent(ctx, string(org), ref, false); err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
							_, err := w.PutReceipt(ctx, receipt)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					for _, expected := range receipts {
						stored, err := r.Receipt(ctx, expected.ActorID, expected.Operation, expected.CommandKey)
						if err != nil {
							return err
						}
						if stored.PayloadObjectKey != expected.PayloadObjectKey || stored.PayloadSHA256 != expected.PayloadSHA256 || stored.PayloadBytes != expected.PayloadBytes || stored.PayloadContentBytes != expected.PayloadContentBytes {
							t.Fatalf("%s receipt content identity changed: operation=%s", stage, expected.Operation)
						}
					}
					object, err := r.ArchiveObjectByReference(ctx, shared)
					if err != nil {
						return err
					}
					if object.CollaborationBytes != shared.CompressedSize {
						t.Errorf("shared command content became unbillable: debit=%d want=%d", object.CollaborationBytes, shared.CompressedSize)
					}
					used, err := r.CollaborationBytes(ctx)
					if err != nil || used != initial+total {
						t.Fatalf("%s reclassified history: usage=%d want=%d err=%v", stage, used, initial+total, err)
					}
					var recounted int64
					if err := h.Admin.QueryRowContext(ctx, "SELECT COALESCE(sum(collaboration_bytes),0) FROM archive_objects WHERE organization_id=$1", org).Scan(&recounted); err != nil {
						return err
					}
					if used != recounted {
						t.Fatalf("recounted receipt usage=%d want=%d", used, recounted)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
