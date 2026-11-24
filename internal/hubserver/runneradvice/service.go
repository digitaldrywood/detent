package runneradvice

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubauth"
	"github.com/digitaldrywood/detent/internal/hubserver/reportstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type Deps struct {
	Repository   tenantstore.Repository
	Now          func() time.Time
	Auth         func() *hubauth.Authenticator
	ReadRouting  func(context.Context, hubauth.Input, string) (runnerauth.Runner, error)
	CapacityView func(context.Context, runnerauth.Runner, string, time.Time) (runnerauth.CapacityView, error)
	RecordError  func(error) error
}
type Service struct{ deps Deps }

func New(deps Deps) *Service { return &Service{deps: deps} }

func (s *Service) Authorize(ctx context.Context, reader tenantstore.Reader, scope hubauth.Input) error {
	if scope.Credential.Runner.RunnerID != "" {
		return accessstore.ErrDenied
	}
	if scope.Credential.Hosted == nil {
		current, err := reader.AccessCredential(ctx, scope.Credential.ID)
		if err != nil {
			return err
		}
		if current.Scope != string(hubauth.Admin) || current.NativeOnly {
			return accessstore.ErrDenied
		}
	} else {
		scope.Credential.ManageRunners = true
	}
	return s.deps.Auth().Authorize(ctx, reader, scope, true)
}

func (s *Service) Read(ctx context.Context, scope hubauth.Input, resource, backend string) (any, error) {
	if backend != "" && (runnerauth.CapacityRequest{ExpectedConfigRevision: strings.Repeat("0", 64), Capacity: 1, Backend: backend}).Validate() != nil {
		return nil, hubapi.PublicInvalid("Backend must be a bounded provider identifier")
	}
	if scope.Credential.Runner.RunnerID != "" {
		return nil, accessstore.ErrDenied
	}
	r, err := s.deps.ReadRouting(ctx, scope, resource)
	if err != nil {
		return nil, err
	}
	now := s.deps.Now().UTC()
	var hours []Hour
	var facts reportstore.RunnerCapacityFacts
	to := now.Truncate(time.Hour)
	from := to.Add(-719 * time.Hour)
	err = s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error {
		if err := s.Authorize(ctx, reader, scope); err != nil {
			return err
		}
		segments, err := reader.RunnerHostMetrics(ctx, resource, from, to)
		if err != nil {
			return err
		}
		hours, err = HoursFromSegments(segments)
		if err != nil {
			return err
		}
		capacityReader, ok := reader.(reportstore.RunnerCapacityReader)
		if !ok {
			return errors.New("PostgreSQL runner capacity reader is unavailable")
		}
		facts, err = capacityReader.ReadRunnerCapacityFacts(ctx, reportstore.RunnerCapacityQuery{RunnerID: resource, MachineID: r.MachineID, From: from, To: to, Limit: reportstore.RunnerCapacityFactLimit})
		if err != nil {
			return err
		}
		return err
	})
	if err != nil {
		return nil, s.deps.RecordError(err)
	}
	current, err := s.deps.ReadRouting(ctx, scope, resource)
	if err != nil {
		return nil, err
	}
	if current.Revision != r.Revision || current.MachineID != r.MachineID {
		return nil, hubapi.Conflict(tracker.Revision(current.Revision))
	}
	if err := s.deps.Repository.ReadTenant(ctx, scope.Organization, corestore.Serving, func(reader tenantstore.Reader) error { return s.Authorize(ctx, reader, scope) }); err != nil {
		return nil, s.deps.RecordError(err)
	}
	recommendation, err := FromFacts(r.Routing, hours, facts)
	if err != nil {
		return nil, err
	}
	recommendation.From, recommendation.To = from, to
	view, err := s.deps.CapacityView(ctx, r, backend, now)
	if err != nil {
		return nil, err
	}
	return CapacityRead{CapacityView: view, Recommendation: recommendation, ProviderCapacity: r.ProviderCapacity}, nil
}
