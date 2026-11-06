package hubserver

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/tracker"
)

func newPostgresTestMetrics(t *testing.T, organization tracker.OrganizationID) (*tenantRequestMetrics, error) {
	t.Helper()
	repo := projectRecordPostgres(t)
	return &tenantRequestMetrics{repository: repo, organization: organization, pending: make(map[requestMetricKey]requestLatency), runners: make(map[string]*runnerRequestWindow)}, nil
}
