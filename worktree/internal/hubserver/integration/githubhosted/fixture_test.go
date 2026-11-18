package githubhosted_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type sharedProvider struct{ auth.HostedProvider }
