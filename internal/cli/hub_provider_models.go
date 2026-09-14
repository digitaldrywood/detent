package cli

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/providercapacity"
	"github.com/digitaldrywood/detent/internal/runner"
)

// Per-model detail for the provider capacity reports a runner publishes.
//
// The report itself is written by the operator's collector, which knows the
// account and its concurrency but not the provider's model catalogue: it lists
// identifiers and nothing more. The backends this runner dispatches on do know
// the catalogue — it is the same one automatic model selection reads — so this
// file asks them once, caches the answer, and decorates each report with the
// detail for the models that report already advertises. The client's model and
// effort pickers are the reason it exists (decisions.md section 14): a picker
// cannot offer a model's reasoning efforts that nobody ever published.
//
// Nothing here can make a heartbeat fail or block one. Reading a catalogue
// means starting a provider's app-server, which is far too slow to sit in the
// claim path, so the read happens on its own goroutine and `decorate` answers
// from whatever the cache holds. A runner that has never completed a read
// publishes exactly the reports it published before this file existed.

const (
	// providerModelCatalogTTL is how long a read is served before another is
	// started. A provider's catalogue changes on the order of weeks.
	providerModelCatalogTTL = 30 * time.Minute
	// providerModelCatalogRetry is how soon a failed read is attempted again.
	// It is short enough that a runner started before its provider was logged
	// in recovers within a few heartbeats.
	providerModelCatalogRetry = 2 * time.Minute
	// providerModelCatalogTimeout bounds one read of every backend catalogue.
	providerModelCatalogTimeout = 60 * time.Second
)

// backendModelCatalog is one backend's catalogue as the report needs it.
type backendModelCatalog struct {
	// Provider names the vendor the backend talks to, e.g. "openai".
	Provider string
	// Effort is this runner's configured effort for the code role, which is
	// the effort a turn it takes would actually run at.
	Effort string
	Models []runner.AgentModel
}

// providerModelCatalog caches the backend catalogues and decorates reports
// with them. The zero value is not usable; build one with
// newProviderModelCatalog.
type providerModelCatalog struct {
	read   func(context.Context) (map[string]backendModelCatalog, error)
	ttl    time.Duration
	retry  time.Duration
	now    func() time.Time
	logger *slog.Logger

	mu       sync.Mutex
	catalogs map[string]backendModelCatalog
	// nextRead is when a read may start again. Zero means "now".
	nextRead time.Time
	reading  bool
}

func newProviderModelCatalog(read func(context.Context) (map[string]backendModelCatalog, error), logger *slog.Logger) *providerModelCatalog {
	if logger == nil {
		logger = slog.Default()
	}
	return &providerModelCatalog{
		read: read, ttl: providerModelCatalogTTL, retry: providerModelCatalogRetry,
		now: time.Now, logger: logger,
	}
}

// decorate returns the reports with per-model detail filled in from the cached
// catalogues, and starts a read when the cache is empty or stale. It never
// blocks on a read and never fails: a report it knows nothing about is
// returned untouched.
func (c *providerModelCatalog) decorate(reports []providercapacity.Report) []providercapacity.Report {
	if c == nil {
		return reports
	}
	catalogs := c.cached()
	for index, report := range reports {
		catalog, ok := catalogs[report.Backend]
		if !ok {
			continue
		}
		details := runner.ProviderModelDetails(firstNonBlankString(catalog.Provider, report.Provider), report.Models, catalog.Effort, catalog.Models)
		if len(details) == 0 {
			continue
		}
		reports[index].ModelDetails = details
	}
	return reports
}

// cached returns the catalogues held now, starting a refresh when the held
// ones have expired. The caller gets the map it may read without the lock
// because a refresh replaces the map rather than writing into it.
func (c *providerModelCatalog) cached() map[string]backendModelCatalog {
	c.mu.Lock()
	defer c.mu.Unlock()
	held := c.catalogs
	if !c.reading && !c.now().Before(c.nextRead) {
		c.reading = true
		go c.refresh()
	}
	return held
}

// refresh reads every backend catalogue once and replaces the cache. A failed
// read keeps whatever the cache already held: a catalogue the runner read an
// hour ago is a far better answer than none.
func (c *providerModelCatalog) refresh() {
	ctx, cancel := context.WithTimeout(context.Background(), providerModelCatalogTimeout)
	defer cancel()
	catalogs, err := c.read(ctx)
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reading = false
	if err != nil {
		c.nextRead = c.now().Add(c.retry)
		c.logger.Debug("provider model catalogue is unavailable; reports keep their model identifiers", "error", err)
		return
	}
	c.catalogs = catalogs
	c.nextRead = c.now().Add(c.ttl)
}

// configuredBackendModelCatalogs reads the model catalogue of every agent
// backend the configured projects dispatch on.
//
// Backends are keyed by their identifier because that is what a provider
// capacity report's `backend` names and what dispatch matches against. The
// first project to describe a backend wins: two projects that share a backend
// identifier share the backend, and asking the same provider twice would only
// start a second app-server.
func configuredBackendModelCatalogs(ctx context.Context, cfg globalconfig.Config) (map[string]backendModelCatalog, error) {
	catalogs := map[string]backendModelCatalog{}
	var failures []error
	for _, projectConfig := range cfg.Projects {
		workflow, err := project.LoadWorkflowContext(ctx, projectConfig)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		effort, _ := workflow.Config.Agent.Effort.Resolve("code")
		for _, backendConfig := range workflow.Config.Agents.Backends {
			id := strings.TrimSpace(backendConfig.ID)
			if backendConfig.Disabled || id == "" {
				continue
			}
			if _, known := catalogs[id]; known {
				continue
			}
			models, err := backendModelCatalogModels(ctx, backendConfig)
			if err != nil {
				failures = append(failures, err)
				continue
			}
			catalogs[id] = backendModelCatalog{Provider: strings.TrimSpace(backendConfig.Provider), Effort: effort, Models: models}
		}
	}
	if len(catalogs) == 0 && len(failures) > 0 {
		return nil, failures[0]
	}
	return catalogs, nil
}

// backendModelCatalogModels builds one backend and asks it for its models. A
// backend that has no catalogue — a protocol that cannot be asked — answers
// with none rather than an error, because there is nothing wrong with it.
func backendModelCatalogModels(ctx context.Context, backendConfig workflowconfig.AgentBackend) ([]runner.AgentModel, error) {
	backend, err := buildAgentBackend(backendConfig)
	if err != nil {
		return nil, err
	}
	provider, ok := backend.(runner.AgentModelCatalogProvider)
	if !ok {
		return nil, nil
	}
	return provider.ListModels(ctx)
}
