package boardprojection_test

import (
	"context"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
)

func quietHub(t *testing.T) *hubfixture.Hub {
	t.Helper()
	return hubfixture.OpenDeps(t, hubfixture.Config{RunDetails: hubtest.NewDetails()}, hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}})
}
