package hubserver

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type authorityReaderProbe struct {
	tenantstore.Repository
	input              tenantauth.RequestIdentity
	reads, legacyReads int
	err                error
}

func (p *authorityReaderProbe) ReadTenantAuthority(_ context.Context, input tenantauth.RequestIdentity) (tenantauth.Scope, error) {
	p.reads++
	p.input = input
	return tenantauth.Scope{}, p.err
}

func (p *authorityReaderProbe) ReadTenant(context.Context, tracker.OrganizationID, corestore.Workload, func(tenantstore.Reader) error) error {
	p.legacyReads++
	return errors.New("unexpected legacy authority read")
}

func TestTenantAuthorityReaderFailsClosedWithoutLegacyReads(t *testing.T) {
	for _, test := range []struct {
		name   string
		err    error
		status int
		denied bool
	}{
		{"denied", tenantauth.ErrDenied, http.StatusUnauthorized, true},
		{"outage", tenantauth.ErrUnavailable, http.StatusServiceUnavailable, false},
		{"zero scope", nil, http.StatusUnauthorized, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			probe := &authorityReaderProbe{err: test.err}
			service := &Service{config: Config{TenantRepository: probe, now: time.Now}}
			_, status, err := service.authenticateTenantAPIHash(t.Context(), "first", "proof", "runner", "first")
			if status != test.status || errors.Is(err, auth.ErrHostedIdentity) != test.denied {
				t.Fatalf("status=%d err=%v", status, err)
			}
			if probe.reads != 1 || probe.legacyReads != 0 || probe.input.Organization != "first" || probe.input.CredentialHash != "proof" || probe.input.RenewalRunner != "runner" || probe.input.RenewalOrganization != "first" {
				t.Fatalf("authority calls=%d legacy=%d input organization=%s", probe.reads, probe.legacyReads, probe.input.Organization)
			}
		})
	}
}
