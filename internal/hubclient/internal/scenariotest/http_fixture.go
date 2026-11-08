package scenariotest

import (
	"path/filepath"
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
)

type executionRoundTrip = testsupport.RoundTrip

var hubDatabasePath = func(t testing.TB) string { return filepath.Join(t.TempDir(), "hub.db") }
