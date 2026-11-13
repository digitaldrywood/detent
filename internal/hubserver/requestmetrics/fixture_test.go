package requestmetrics

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }
func newPostgresTestMetrics(t *testing.T, org tracker.OrganizationID) (*Service, error) {
	t.Helper()
	return New(Deps{Repository: hubtest.Repository(t), Organization: org})
}
