package accessreceipts

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

func TestPostgresHostedOperationReceiptsPreservePendingAndReplay(t *testing.T) {
	repository := hubtest.Repository(t)
	now := time.Now().UTC()
	hubtest.Organization(t, repository, hubtest.OrganizationOptions{ID: "org_security", Name: "Example organization", Now: now})
	details := hubtest.NewDetails()
	service := New(Deps{Repository: repository, TenantRepository: repository, Organization: "org_security", Now: time.Now, Details: details, ResolveJSON: func(_ context.Context, raw []byte) ([]byte, error) { return raw, nil }})
	users := []hubtest.SeededMember{
		hubtest.Member(t, repository, "org_security", hubtest.MemberOptions{Role: "member", Email: "first@example.test", Now: now}),
		hubtest.Member(t, repository, "org_security", hubtest.MemberOptions{Role: "member", Email: "second@example.test", Now: now}),
	}
	if service.deps.Repository == nil || service.deps.TenantRepository == nil {
		t.Fatal("required PostgreSQL fixture owners are not configured")
	}
	content := eventstore.NewContent(details)
	for _, user := range users {
		var member accessstore.Member
		if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			member, err = r.AccessMember(t.Context(), user.Member.UserID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		command := Command{Organization: "org_security", Actor: member.PrincipalID, Operation: "example.action", Key: "example-command", Input: map[string]string{"name": "example"}}
		claimed, _, err := service.Claim(t.Context(), command)
		if err != nil || !claimed {
			t.Fatalf("first claim: owned=%t error=%v", claimed, err)
		}
		if claimed, _, err := service.Claim(t.Context(), command); claimed || !errors.Is(err, mutation.ErrUncertain) {
			t.Fatalf("held claim repeated: owned=%t error=%v", claimed, err)
		}
		changed := command
		changed.Input = map[string]string{"name": "different"}
		if _, _, err := service.Claim(t.Context(), changed); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("changed request error=%v, want conflict", err)
		}
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		value := map[string]any{"pending": true, "request": map[string]string{"id": "example-request"}}
		raw, err := service.Complete(canceled, command, value)
		if err != nil {
			t.Fatalf("completion after cancellation: %v", err)
		}
		claimed, replay, err := service.Claim(t.Context(), command)
		if err != nil || claimed || string(replay) != string(raw) {
			t.Fatalf("completed asynchronous replay: owned=%t error=%v", claimed, err)
		}
		var before, after int64
		readUsage := func(target *int64) {
			t.Helper()
			if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				*target, err = r.ArchiveBytes(t.Context())
				return err
			}); err != nil {
				t.Fatal(err)
			}
		}
		readUsage(&before)
		repeated, err := service.Complete(t.Context(), command, map[string]bool{"different": true})
		if err != nil || string(repeated) != string(raw) {
			t.Fatalf("duplicate completion replaced outcome: %v", err)
		}
		abandon(t, service, command)
		readUsage(&after)
		if after != before {
			t.Fatalf("duplicate completion or abandon changed archive charge: before=%d after=%d", before, after)
		}
		expired := command
		expired.Key = "expired-command"
		hash, err := hubapi.CommandHash(expired.Input)
		if err != nil {
			t.Fatal(err)
		}
		record, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_security", ActorID: command.Actor, Operation: expired.Operation, CommandKey: expired.Key, RequestHash: hash, CreatedAt: now, ResponseExpired: true}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.PutReceipt(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := service.Claim(t.Context(), expired); !errors.Is(err, receiptstore.ErrExpired) {
			t.Fatalf("expired command became claimable: %v", err)
		}
		abandoned := command
		abandoned.Key = "rejected-command"
		if claimed, _, err := service.Claim(t.Context(), abandoned); err != nil || !claimed {
			t.Fatalf("rejected command claim: owned=%t error=%v", claimed, err)
		}
		abandon(t, service, abandoned)
		if _, found, err := service.Read(t.Context(), abandoned); err != nil || found {
			t.Fatalf("definitely rejected claim survived: found=%t error=%v", found, err)
		}
		if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessMember(t.Context(), user.Member.UserID, now.Add(time.Second))
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := service.Read(t.Context(), command); !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("revoked member read command receipt: %v", err)
		}
	}
}

func TestMain(m *testing.M) { hubtest.Main(m) }

func abandon(t *testing.T, service *Service, command Command) {
	t.Helper()
	if err := service.Abandon(t.Context(), command); err != nil {
		t.Error(err)
	}
}
