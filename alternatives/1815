package hubserver

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/mutation"
)

func TestPostgresHostedOperationReceiptsPreservePendingAndReplay(t *testing.T) {
	repository := projectRecordPostgres(t)
	provider := newHostedSecurityProvider()
	service := &Service{echo: echo.New(), database: &database{postgres: repository}, secretRewrapReady: make(chan struct{}), config: Config{Logger: discardLogger(), now: time.Now, generateToken: apikey.GenerateToken, AccessRepository: repository, TenantRepository: repository, RunDetails: &memoryRunDetails{}, Hosted: &HostedConfig{OrganizationID: "org_security", WorkOSOrganizationID: "org_provider", PublicURL: "http://127.0.0.1:7777", Provider: provider}}}
	sessions, err := auth.NewSessionService(auth.SessionConfig{SessionTTL: 24 * time.Hour, PublicURL: service.config.Hosted.PublicURL}, service)
	if err != nil {
		t.Fatal(err)
	}
	service.hostedSessions = sessions
	service.registerRoutes(service.echo)
	f := hostedSecurityFixture{service: service, provider: provider}
	now := time.Now().UTC()
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: "org_security", Name: "Example organization", CreatedAt: now}); err != nil {
			return err
		}
		return w.CreateAccessTenant(t.Context(), accessstore.Tenant{OrganizationID: "org_security", ProviderID: "org_provider"})
	}); err != nil {
		t.Fatal(err)
	}
	users := []hostedSecurityUser{
		f.user(t, "first_receipt", "member", "first@example.test", "", ""),
		f.user(t, "second_receipt", "member", "second@example.test", "", ""),
	}
	if service.database.db != nil || service.database.reader != nil || service.database.authReader != nil || service.database.claimReader != nil {
		t.Fatal("receipt fixture retained a legacy database handle")
	}
	content := eventstore.NewContent(f.service.runDetailStore())
	for _, user := range users {
		var member accessstore.Member
		if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			member, err = r.AccessMember(t.Context(), user.identity.Subject)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		command := hostedCommand{actor: member.PrincipalID, operation: "example.action", key: "example-command", input: map[string]string{"name": "example"}}
		claimed, _, err := f.service.claimHostedOperation(t.Context(), command)
		if err != nil || !claimed {
			t.Fatalf("first claim: owned=%t error=%v", claimed, err)
		}
		if claimed, _, err := f.service.claimHostedOperation(t.Context(), command); claimed || !errors.Is(err, mutation.ErrUncertain) {
			t.Fatalf("held claim repeated: owned=%t error=%v", claimed, err)
		}
		changed := command
		changed.input = map[string]string{"name": "different"}
		if _, _, err := f.service.claimHostedOperation(t.Context(), changed); !errors.Is(err, mutation.ErrConflict) {
			t.Fatalf("changed request error=%v, want conflict", err)
		}
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		value := map[string]any{"pending": true, "request": map[string]string{"id": "example-request"}}
		raw, err := f.service.completeHostedOperation(canceled, command, value)
		if err != nil {
			t.Fatalf("completion after cancellation: %v", err)
		}
		claimed, replay, err := f.service.claimHostedOperation(t.Context(), command)
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
		repeated, err := f.service.completeHostedOperation(t.Context(), command, map[string]bool{"different": true})
		if err != nil || string(repeated) != string(raw) {
			t.Fatalf("duplicate completion replaced outcome: %v", err)
		}
		f.service.abandonHostedOperation(t.Context(), command)
		readUsage(&after)
		if after != before {
			t.Fatalf("duplicate completion or abandon changed archive charge: before=%d after=%d", before, after)
		}
		expired := command
		expired.key = "expired-command"
		hash, err := expired.hash()
		if err != nil {
			t.Fatal(err)
		}
		record, err := content.PrepareReceipt(t.Context(), eventstore.Receipt{OrganizationID: "org_security", ActorID: command.actor, Operation: expired.operation, CommandKey: expired.key, RequestHash: hash, CreatedAt: now, ResponseExpired: true}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.PutReceipt(t.Context(), record)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.service.claimHostedOperation(t.Context(), expired); !errors.Is(err, receiptstore.ErrExpired) {
			t.Fatalf("expired command became claimable: %v", err)
		}
		response := httptest.NewRecorder()
		if err := f.service.nativeAPIError(f.service.echo.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), response), receiptstore.ErrExpired); err != nil || response.Code != http.StatusGone {
			t.Fatalf("expired replay HTTP status=%d error=%v", response.Code, err)
		}
		abandoned := command
		abandoned.key = "rejected-command"
		if claimed, _, err := f.service.claimHostedOperation(t.Context(), abandoned); err != nil || !claimed {
			t.Fatalf("rejected command claim: owned=%t error=%v", claimed, err)
		}
		f.service.abandonHostedOperation(t.Context(), abandoned)
		if _, found, err := f.service.readHostedOperation(t.Context(), abandoned); err != nil || found {
			t.Fatalf("definitely rejected claim survived: found=%t error=%v", found, err)
		}
		if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			return w.RevokeAccessMember(t.Context(), user.identity.Subject, now.Add(time.Second))
		}); err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.service.readHostedOperation(t.Context(), command); !errors.Is(err, accessstore.ErrDenied) {
			t.Fatalf("revoked member read command receipt: %v", err)
		}
	}
}
