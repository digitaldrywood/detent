package workpage_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type organization struct {
	ID tracker.OrganizationID `json:"organization_id"`
}
