package hubserver

import (
	"errors"
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/tenantauth"
)

func TestPostgresBillingBindingLookupOutages(t *testing.T) {
	f := newSharedRouteFixture(t, sharedRouteBillingConfig)
	for org, service := range f.services {
		repository := service.database.billingRepository
		for _, test := range []struct {
			name string
			err  error
			want int
		}{
			{"storage", errors.New("private-outage-sentinel"), http.StatusServiceUnavailable},
			{"outage before absence", errors.Join(tenantauth.ErrUnavailable, corestore.ErrNotFound), http.StatusServiceUnavailable},
			{"unbound", corestore.ErrNotFound, http.StatusConflict},
		} {
			t.Run(org+"/"+test.name, func(t *testing.T) {
				service.database.billingRepository = runnerGrantOutageRepository{Repository: repository, err: test.err}
				defer func() { service.database.billingRepository = repository }()
				response := f.request(t, org, "/internal/v1/billing/events", `{"event_id":"evt_example","event_type":"invoice.paid","customer_id":"cus_`+org+`"}`, nil)
				requireEntryAuthResponse(t, response.Code, response.Body.String(), test.want)
			})
		}
	}
}
