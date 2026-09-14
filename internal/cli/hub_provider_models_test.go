package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
)

// catalogReader is a scripted stand-in for the backend catalogue read, which
// in production starts a provider's app-server.
type catalogReader struct {
	mu       sync.Mutex
	calls    int
	done     chan struct{}
	catalogs map[string]backendModelCatalog
	err      error
}

func (r *catalogReader) read(context.Context) (map[string]backendModelCatalog, error) {
	r.mu.Lock()
	r.calls++
	catalogs, err := r.catalogs, r.err
	done := r.done
	r.mu.Unlock()
	if done != nil {
		close(done)
	}
	return catalogs, err
}

func (r *catalogReader) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func capacityReportFixture() providercapacity.Report {
	return providercapacity.Report{
		Provider: "openai", Backend: "codex", AccountAlias: "work",
		Models: []string{"astra", "sol"}, MaxConcurrent: 2,
		Availability: "available", ObservedAt: time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC),
	}
}

// waitForCatalog blocks until the catalogue read the previous decorate started
// has finished and been stored, so the assertions that follow are not racing
// the goroutine.
func waitForCatalog(t *testing.T, reader *catalogReader, catalog *providerModelCatalog) {
	t.Helper()
	select {
	case <-reader.done:
	case <-time.After(5 * time.Second):
		t.Fatal("the catalogue read never started")
	}
	for range 500 {
		catalog.mu.Lock()
		reading := catalog.reading
		catalog.mu.Unlock()
		if !reading {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("the catalogue read never finished")
}

// TestProviderModelCatalogDecorates covers the whole point of the cache: the
// first report is published on time with no detail, the catalogue read happens
// off the claim path, and the next report carries the detail the pickers need
// (decisions section 14).
func TestProviderModelCatalogDecorates(t *testing.T) {
	t.Parallel()
	reader := &catalogReader{
		done: make(chan struct{}),
		catalogs: map[string]backendModelCatalog{"codex": {
			Provider: "openai", Effort: "high",
			Models: []runner.AgentModel{
				{ID: "astra", Model: "astra", Default: true, SupportedReasoningEfforts: []string{"low", "high"}},
				{ID: "sol", Model: "sol", Upgrade: "astra", SupportedReasoningEfforts: []string{"low"}},
			},
		}},
	}
	catalog := newProviderModelCatalog(reader.read, quietLogger())

	first := catalog.decorate([]providercapacity.Report{capacityReportFixture()})
	if first[0].ModelDetails != nil {
		t.Fatalf("the first report waited for the catalogue: %#v", first[0].ModelDetails)
	}
	waitForCatalog(t, reader, catalog)

	second := catalog.decorate([]providercapacity.Report{capacityReportFixture()})
	if err := providercapacity.Validate(second); err != nil {
		t.Fatalf("the decorated report is not publishable: %v", err)
	}
	astra, ok := second[0].Detail("astra")
	if !ok || !astra.Default || astra.Legacy || astra.DefaultReasoningEffort != "high" ||
		!slices.Equal(astra.ReasoningEfforts, []string{"low", "high"}) {
		t.Fatalf("astra detail = %#v, %t", astra, ok)
	}
	sol, ok := second[0].Detail("sol")
	if !ok || !sol.Legacy || sol.DefaultReasoningEffort != "" {
		t.Fatalf("sol detail = %#v, %t", sol, ok)
	}
}

// TestProviderModelCatalogSurvivesAFailedRead proves a runner whose provider
// cannot be asked still publishes the reports its collector wrote, and tries
// again rather than giving up.
func TestProviderModelCatalogSurvivesAFailedRead(t *testing.T) {
	t.Parallel()
	reader := &catalogReader{done: make(chan struct{}), err: errors.New("codex is not logged in")}
	catalog := newProviderModelCatalog(reader.read, quietLogger())
	catalog.retry = 0

	reports := catalog.decorate([]providercapacity.Report{capacityReportFixture()})
	if len(reports) != 1 || reports[0].ModelDetails != nil {
		t.Fatalf("reports = %#v, want the collector's own report", reports)
	}
	waitForCatalog(t, reader, catalog)

	reader.mu.Lock()
	reader.done = make(chan struct{})
	reader.mu.Unlock()
	if again := catalog.decorate([]providercapacity.Report{capacityReportFixture()}); again[0].ModelDetails != nil {
		t.Fatalf("a failed read invented detail: %#v", again[0].ModelDetails)
	}
	waitForCatalog(t, reader, catalog)
	if reader.count() < 2 {
		t.Fatalf("reads = %d, want the failed read retried", reader.count())
	}
}

// TestProviderModelCatalogHoldsItsRead proves a fresh catalogue is served from
// memory: the read starts a provider process, so a heartbeat every few seconds
// must not start one.
func TestProviderModelCatalogHoldsItsRead(t *testing.T) {
	t.Parallel()
	reader := &catalogReader{done: make(chan struct{}), catalogs: map[string]backendModelCatalog{}}
	catalog := newProviderModelCatalog(reader.read, quietLogger())

	catalog.decorate([]providercapacity.Report{})
	waitForCatalog(t, reader, catalog)
	for range 5 {
		catalog.decorate([]providercapacity.Report{capacityReportFixture()})
	}
	if got := reader.count(); got != 1 {
		t.Fatalf("reads = %d, want the held catalogue reused", got)
	}
}

// TestProviderModelCatalogLeavesUnknownBackendsAlone proves detail is only
// ever attached to the backend that reported it: a report from a backend this
// runner does not dispatch on is published untouched.
func TestProviderModelCatalogLeavesUnknownBackendsAlone(t *testing.T) {
	t.Parallel()
	reader := &catalogReader{
		done: make(chan struct{}),
		catalogs: map[string]backendModelCatalog{"claude": {
			Provider: "anthropic",
			Models:   []runner.AgentModel{{ID: "opus", Model: "opus"}},
		}},
	}
	catalog := newProviderModelCatalog(reader.read, quietLogger())
	catalog.decorate([]providercapacity.Report{})
	waitForCatalog(t, reader, catalog)

	reports := catalog.decorate([]providercapacity.Report{capacityReportFixture()})
	if reports[0].ModelDetails != nil {
		t.Fatalf("detail = %#v, want none for an unreported backend", reports[0].ModelDetails)
	}
}

// TestProviderModelCatalogIsOptional proves the nil catalogue — a runner with
// no capacity file — is a no-op rather than a panic.
func TestProviderModelCatalogIsOptional(t *testing.T) {
	t.Parallel()
	var catalog *providerModelCatalog
	reports := []providercapacity.Report{capacityReportFixture()}
	if got := catalog.decorate(reports); len(got) != 1 || got[0].ModelDetails != nil {
		t.Fatalf("decorate() = %#v", got)
	}
}
