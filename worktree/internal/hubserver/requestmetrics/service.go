package requestmetrics

import (
	"context"
	"errors"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/metricsstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository   metricsstore.Repository
	Organization tracker.OrganizationID
}

func New(deps Deps) (*Service, error) {
	if deps.Repository == nil || deps.Organization == "" {
		return nil, errors.New("hosted database binding is required")
	}
	return &Service{Repository: deps.Repository, Organization: deps.Organization, pending: make(map[Key]Latency), runners: make(map[string]*RunnerWindow)}, nil
}
func (m *Service) RecordUsage(owner *BillingOwner, key Key, usage Usage) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.usage = owner
	value := m.pending[key]
	value.HTTP.Merge(usage)
	m.pending[key] = value
}
func (m *Service) Retain(ctx context.Context, now time.Time) error {
	return m.Repository.RetainRequestMetrics(ctx, m.Organization, now)
}
