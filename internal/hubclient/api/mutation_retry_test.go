package api_test

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/hubclient/internal/scenariotest"
)

func TestNativeMutationRetryAfterResponseLoss(t *testing.T) {
	scenariotest.RunNativeMutationRetryAfterResponseLoss(t)
}
