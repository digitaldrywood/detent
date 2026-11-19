package workitemchange_test

import (
	"net/http"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
)

func TestReleaseDeliveryStatusPostgresMissingItem(t *testing.T) {
	t.Parallel()
	fixture := hubfixture.NewNativeInOrganization(t, nil)
	response := fixture.Request(t, http.MethodGet, fixture.Base+"/work-items/wi_missing/release-pick", nil)
	hubfixture.RequireStatus(t, response, http.StatusNotFound)
}
