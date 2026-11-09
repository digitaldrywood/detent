package hubserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestCommandPayloadRetentionNeverRepeatsEffects(t *testing.T) {
	start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	repository, admin := projectRecordPostgresWithAdmin(t, 4)
	s := &Service{config: Config{TenantRepository: repository, AccessRepository: repository, RunDetails: &memoryRunDetails{}, now: func() time.Time { return time.Unix(0, clock.Load()).UTC() }}}
	content := eventstore.NewContent(s.runDetailStore())
	for _, organization := range []tracker.OrganizationID{"org_first", "org_second"} {
		actor := "principal_" + string(organization)
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: organization, Name: "Example organization", CreatedAt: start}); err != nil {
				return err
			}
			return w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: actor, Hash: actor, Name: "Example principal", Scope: "admin", Kind: "service", ProjectAccess: "all", CreatedAt: start, UpdatedAt: start})
		}); err != nil {
			t.Fatal(err)
		}
		s.localOrganization = string(organization)
		for _, operation := range []string{"POST /example", "PUT /example", "PATCH /example", "DELETE /example", "runner_routing example-runner", "runner_capacity example-runner", "runner_update example-runner", "urgent_runner_update", "drain_local_project example-runner example-project", "resume_local_project example-runner example-project", nativeOperation(nativeScope{organization: organization, project: "example-project"}, "POST", "/conversations/example-conversation/messages"), "mcp file_issue", "attachment.record example-attachment"} {
			t.Run(string(organization)+"/"+operation, func(t *testing.T) {
				clock.Store(start.UnixNano())
				ctx := t.Context()
				input := map[string]string{"effect": "example"}
				hash, err := nativeCommandHash(input)
				if err != nil {
					t.Fatal(err)
				}
				scope := nativeScope{organization: organization, credential: apiCredential{ID: actor, Hash: actor, Scope: apiScopeAdmin}}
				raw := fmt.Sprintf(`{"organization":%q,"output":%q}`, organization, strings.Repeat("x", 64<<10))
				command := hostedCommand{organization: string(organization), actor: actor, operation: operation, key: "external", input: input}
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
				for _, key := range []string{"native", "pending", "uncertain", "legacy"} {
					record := eventstore.Receipt{OrganizationID: organization, ActorID: actor, Operation: operation, CommandKey: key, RequestHash: hash, CreatedAt: start, ResponseReplayUntilNs: start.Add(receiptstore.ReplayRetention).UnixNano()}
					if key == "native" {
						record.Operation += " example-session"
					}
					if key == "uncertain" {
						record.ResponseClaimed, record.ResponsePending = true, true
						if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error {
							_, _, err := w.(receiptstore.Writer).ClaimReceipt(ctx, record)
							return err
						}); err != nil {
							t.Fatal(err)
						}
						continue
					}
					body := raw
					if key == "pending" {
						body = `{"pending":true}`
					}
					record, err = content.PrepareReceipt(ctx, record, []byte(body))
					if err != nil {
						t.Fatal(err)
					}
					if err := repository.WriteTenant(ctx, organization, corestore.Serving, func(w tenantstore.Writer) error { _, err := w.PutReceipt(ctx, record); return err }); err != nil {
						t.Fatal(err)
					}
				}
				requireCommandPayloadObject(t, s, organization, operation, "external", []byte(raw), actor)
				requireCommandPayloadObject(t, s, organization, operation+" example-session", "native", []byte(raw), actor)
				archiveOwners := func() int {
					t.Helper()
					var count int
					if err := admin.QueryRowContext(ctx, `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND owner_kind='native-command'`, organization).Scan(&count); err != nil {
						t.Fatal(err)
					}
					return count
				}
				ownersBeforeExpiry := archiveOwners()
				for _, age := range []time.Duration{0, 24 * time.Hour, 24*time.Hour + time.Second} {
					clock.Store(start.Add(age).UnixNano())
					if err := s.retainCommandPayloads(ctx, s.config.now()); err != nil {
						t.Fatal(err)
					}
					wantOwners := ownersBeforeExpiry
					if age > receiptstore.ReplayRetention {
						if operation == "urgent_runner_update" {
							wantOwners--
						} else {
							wantOwners -= 3
						}
					}
					if owners := archiveOwners(); owners != wantOwners {
						t.Fatalf("age=%s command archive owners=%d, want %d", age, owners, wantOwners)
					}
					response, err := run()
					expires := age > receiptstore.ReplayRetention && operation != "urgent_runner_update"
					if expires {
						if !errors.Is(err, receiptstore.ErrExpired) {
							t.Fatalf("external replay did not return tombstone: %v", err)
						}
					} else if err != nil || string(response) != raw {
						t.Fatalf("external replay lost body: %v", err)
					}
					if effects != 1 {
						t.Fatalf("external effect repeated %d times", effects)
					}
					for _, key := range []string{"external", "native", "legacy"} {
						storedOperation := operation
						if key == "native" {
							storedOperation += " example-session"
						}
						var stored eventstore.Receipt
						if err := repository.ReadTenant(ctx, organization, corestore.Serving, func(r tenantstore.Reader) error {
							var err error
							stored, err = r.Receipt(ctx, actor, storedOperation, key)
							return err
						}); err != nil {
							t.Fatalf("age=%s key=%s lost tombstone: %v", age, key, err)
						}
						wantExpired := age > receiptstore.ReplayRetention && storedOperation != "urgent_runner_update"
						response, found, err := s.nativeCommandReplay(ctx, scope, operation, key, input)
						if !found {
							t.Fatalf("age=%s key=%s replay missing", age, key)
						}
						if wantExpired {
							if !stored.ResponseExpired || !errors.Is(err, receiptstore.ErrExpired) {
								t.Fatalf("key=%s expiry state=%v error=%v", key, stored.ResponseExpired, err)
							}
						} else if stored.ResponseExpired || err != nil || string(response) != raw {
							t.Fatalf("age=%s key=%s replay lost body: %v", age, key, err)
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

func requireCommandPayloadObject(t *testing.T, s *Service, organization tracker.OrganizationID, operation, key string, want []byte, actors ...string) {
	t.Helper()
	actor := "test_hub_admin"
	if len(actors) > 0 {
		actor = actors[0]
	}
	var receipt eventstore.Receipt
	if err := s.config.TenantRepository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		receipt, err = r.Receipt(t.Context(), actor, operation, key)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if receipt.PayloadObjectKey == "" {
		t.Fatalf("%s has no immutable response object", operation)
	}
	raw, err := eventstore.NewContent(s.runDetailStore()).Receipt(t.Context(), receipt)
	if err != nil || !bytes.Equal(bytes.TrimSpace(raw), bytes.TrimSpace(want)) {
		t.Fatalf("%s object lost response: %v", operation, err)
	}
}
