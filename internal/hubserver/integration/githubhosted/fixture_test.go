package githubhosted_test

import (
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"testing"
)

func TestMain(m *testing.M) { hubtest.Main(m) }

type sharedProvider struct{ auth.HostedProvider }
