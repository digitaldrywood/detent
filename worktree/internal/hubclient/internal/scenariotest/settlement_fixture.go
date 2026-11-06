package scenariotest

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type SettlementChecks struct {
	Observe           func(context.Context, runner.Execution) error
	SetConversation   func(runner.Execution)
	DisableBatching   func(runner.Execution)
	ReplayPublication func(context.Context, runner.Execution) error
	TamperPublication func(runner.Execution, string)
	StoreDiff         func(context.Context, runner.Execution, int64) error
	HasDiffSource     func(runner.Execution) bool
}

func FixtureHead(label string) string { return nativeChangeFixtureHead(label) }
func FixtureDiff(head string, files ...string) runner.AttemptDiffSource {
	return nativeChangeDiff(head, files...)
}
func (e *ownedExecution) finalizationSource() *tracker.NativeChangeReference {
	v := e.CompletionSnapshot().PreparedFinalization
	if v == nil {
		return nil
	}
	return v.SourceVersion
}

type executionTransport struct {
	next http.RoundTripper
	drop atomic.Bool
	down atomic.Bool
}

func (t *executionTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.down.Load() {
		return nil, errors.New("injected Hub outage")
	}
	response, err := t.next.RoundTrip(request)
	if err == nil && strings.HasSuffix(request.URL.Path, "/events") && t.drop.Swap(false) {
		if closeErr := response.Body.Close(); closeErr != nil {
			return nil, closeErr
		}
		return nil, errors.New("injected acknowledgment loss after commit")
	}
	return response, err
}
