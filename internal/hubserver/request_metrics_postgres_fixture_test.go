package hubserver

import (
	"testing"
"github.com/digitaldrywood/detent/internal/hubserver/requestmetrics"
"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func newPostgresTestMetrics(t *testing.T, org tracker.OrganizationID) (*tenantRequestMetrics,error){t.Helper();return requestmetrics.New(requestmetrics.Deps{Repository:projectRecordPostgres(t),Organization:org})}
func newTenantTestMetrics(repo metricsstore.Repository,org tracker.OrganizationID)*tenantRequestMetrics{m,err:=requestmetrics.New(requestmetrics.Deps{Repository:repo,Organization:org});if err!=nil{panic(err)};return m}
