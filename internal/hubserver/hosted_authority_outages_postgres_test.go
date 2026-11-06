package hubserver

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/mutation"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func TestPostgresHostedUsageAuthorityOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.AccessRepository
		for _, fault := range []struct {
			name, message string
			err           error
		}{
			{"storage", "Operator tool is unavailable", errors.New("private-outage-sentinel")},
			{"outage before denial", "Operator tool is unavailable", errors.Join(tenantauth.ErrUnavailable, accessstore.ErrDenied)},
			{"denial", operatortool.ErrAccessDenied.Error(), accessstore.ErrDenied},
		} {
			t.Run(org+"/"+fault.name, func(t *testing.T) {
				service.config.AccessRepository = hostedUsageFaultRepository{Repository: repository, err: fault.err}
				defer func() { service.config.AccessRepository = repository }()
				body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "monthly_usage_costs", "arguments": map[string]string{"scope": "organization", "month": time.Now().UTC().Format("2006-01")}, "_meta": json.RawMessage(fleetProtocolMeta)}})
				if err != nil {
					t.Fatal(err)
				}
				response := hostedOutageRequest(t, f, org, http.MethodPost, "/api/v2/organizations/"+org+"/mcp", string(body), "application/json")
				requireEntryAuthResponse(t, response.Code, response.Body.String(), http.StatusOK)
				var envelope struct {
					Result struct {
						IsError bool `json:"isError"`
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
					} `json:"result"`
				}
				decodeHubResponse(t, response, &envelope)
				if !envelope.Result.IsError || len(envelope.Result.Content) != 1 || !strings.HasPrefix(envelope.Result.Content[0].Text, fault.message) {
					t.Fatalf("body=%s want message=%s", response.Body, fault.message)
				}
			})
		}
	}
}

func TestPostgresHostedProjectWriteOutages(t *testing.T) {
	f := newSharedRouteFixture(t)
	for org, service := range f.services {
		repository := service.config.TenantRepository
		for _, fault := range []struct {
			name string
			err  error
			want int
		}{
			{"storage", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
			{"outage before conflict", errors.Join(tenantauth.ErrUnavailable, mutation.ErrConflict), http.StatusServiceUnavailable},
			{"conflict", mutation.ErrConflict, http.StatusConflict},
			{"denial", accessstore.ErrDenied, http.StatusForbidden},
		} {
			t.Run(org+"/"+fault.name, func(t *testing.T) {
				service.config.TenantRepository = entryAuthFaultRepository{Repository: repository, stage: "write", err: fault.err}
				defer func() { service.config.TenantRepository = repository }()
				response := hostedOutageRequest(t, f, org, http.MethodPost, "/organizations/"+org+"/projects", "name=Example&grant_access=true", "application/x-www-form-urlencoded")
				requireEntryAuthResponse(t, response.Code, response.Body.String(), fault.want)
			})
		}
	}
}
