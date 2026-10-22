package web

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/hub"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/telemetry"
)

func TestAnalyticsUnavailableServices(t *testing.T) {
	identity := operatortool.Identity{PrincipalID: "test", OrganizationID: "self-hosted", CredentialID: "test"}
	ctx := operatortool.WithConnection(t.Context(), operatortool.Connection{Identity: identity, Resolve: func(context.Context) (operatortool.Authority, error) {
		return operatortool.Authority{Identity: identity, Check: func(_ context.Context, r operatortool.Requirement) error {
			if r.Scope != apikey.ScopeRead {
				t.Fatalf("analytics requested scope %s", r.Scope)
			}
			return nil
		}}, nil
	}})
	server := &Server{now: time.Now}
	for _, name := range []string{operatortool.AnalyticsDashboard, operatortool.TimeSeries, operatortool.Reports} {
		t.Run(name, func(t *testing.T) {
			_, err := server.executeAnalyticsRead(ctx, operatortool.Call{Name: name, Arguments: []byte(`{}`)})
			if !errors.Is(err, errOperatorCommandUnavailable) {
				t.Fatalf("missing service error=%v", err)
			}
			_, err = server.executeAnalyticsRead(t.Context(), operatortool.Call{Name: name, Arguments: []byte(`{}`)})
			if !errors.Is(err, operatortool.ErrAccessDenied) {
				t.Fatalf("missing authority error=%v", err)
			}
		})
	}
	server.hub = hub.New[telemetry.Snapshot]()
	if err := server.hub.Publish(telemetry.Snapshot{GeneratedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_, err := server.executeAnalyticsRead(ctx, operatortool.Call{Name: operatortool.Reports, Arguments: []byte(`{}`)})
	if !errors.Is(err, errOperatorCommandUnavailable) {
		t.Fatalf("missing store error=%v", err)
	}
}
