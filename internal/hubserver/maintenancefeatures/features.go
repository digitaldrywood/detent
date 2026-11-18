package maintenancefeatures

import (
	"context"
	"sync"
	"time"

	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type dueProbe interface {
	MaintenanceDue(context.Context, string, string, time.Time, bool) (bool, error)
}

func ProbeFeatures(features []hubmaintenance.Feature, repository any, now func() time.Time, intake, reconciliation bool) []hubmaintenance.Feature {
	probe, ok := repository.(dueProbe)
	if !ok {
		return features
	}
	for i, feature := range features {
		if feature.Name == "board" || feature.Name == "usage" || feature.Name == "project-pages" {
			continue
		}
		var projects sync.Map
		features[i].Step = func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
			if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
				return hubmaintenance.Outcome{}, err
			}
			if feature.Name == "github-intake" && !intake || feature.Name == "github-reconciliation" && !reconciliation {
				return hubmaintenance.Outcome{}, nil
			}
			var due bool
			var err error
			if feature.Name == "integration-pages" {
				var after string
				if v, ok := projects.Load(tenant.Organization); ok {
					after = v.(string)
				}
				due, err = SweepDue(ctx, repository, tenant.Organization, feature.Name, &after, 1)
				if err == nil {
					projects.Store(tenant.Organization, after)
				}
			} else {
				due, err = probe.MaintenanceDue(ctx, tenant.Organization, feature.Name, now(), intake)
			}
			if err != nil || !due {
				return hubmaintenance.Outcome{}, err
			}
			return feature.Step(ctx, tenant, budget)
		}
	}
	return features
}

func Single(name string, bytes int64, enabled bool, step func(context.Context, tracker.OrganizationID) error) hubmaintenance.Feature {
	return hubmaintenance.Feature{Name: name, Bytes: bytes, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, _ hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return hubmaintenance.Outcome{}, err
		}
		if !enabled {
			return hubmaintenance.Outcome{}, nil
		}
		return hubmaintenance.Outcome{Progress: true, Items: 1}, step(ctx, tracker.OrganizationID(tenant.Organization))
	}}
}

func Service[S interface{ Maintain(context.Context) error }](name string, bytes int64, enabled bool, service func(tracker.OrganizationID) S) hubmaintenance.Feature {
	return Single(name, bytes, enabled, func(ctx context.Context, org tracker.OrganizationID) error { return service(org).Maintain(ctx) })
}

func SweepDue[T ~string](ctx context.Context, repo any, organization, family string, cursor *T, limit int) (bool, error) {
	probe, ok := repo.(interface {
		SweepDue(context.Context, string, string, string, int) (string, bool, error)
	})
	if !ok {
		return true, nil
	}
	next, due, err := probe.SweepDue(ctx, organization, family, string(*cursor), limit)
	if err == nil && !due {
		*cursor = T(next)
	}
	return due, err
}

func Board(name string, bytes int64, dirtyOnly bool, batch int, repo any, process func(context.Context, tracker.OrganizationID, int64, bool, tracker.NativeWorkItemID, ...tracker.NativeWorkItemID) (tracker.NativeWorkItemID, bool, bool, error)) hubmaintenance.Feature {
	type position struct {
		generation int64
		after      tracker.NativeWorkItemID
	}
	var sweep sync.Map
	return hubmaintenance.Feature{Name: name, Bytes: bytes, Lead: dirtyOnly, Step: func(ctx context.Context, tenant hubmaintenance.Tenant, budget hubmaintenance.Budget) (hubmaintenance.Outcome, error) {
		if err := hubmaintenance.RequireTenant(ctx, tenant); err != nil {
			return hubmaintenance.Outcome{}, err
		}
		if err := hubmaintenance.RequireAllowance(ctx, tenant.Organization, bytes); err != nil {
			return hubmaintenance.Outcome{}, err
		}
		var after tracker.NativeWorkItemID
		if !dirtyOnly {
			if pos, ok := sweep.Load(tenant.Organization); ok && pos.(position).generation == tenant.Generation {
				after = pos.(position).after
			}
			defer func() { sweep.Store(tenant.Organization, position{tenant.Generation, after}) }()
			due, err := SweepDue(ctx, repo, tenant.Organization, "board", &after, 1)
			if err != nil {
				return hubmaintenance.Outcome{}, err
			}
			if !due {
				if after == "" {
					return hubmaintenance.Outcome{}, nil
				}
				return hubmaintenance.Outcome{Progress: true, Items: 1, CursorOnly: true}, nil
			}
		}
		return hubmaintenance.Drain(ctx, min(budget.Items, batch), &after, func(cursor tracker.NativeWorkItemID, attempted []tracker.NativeWorkItemID) (tracker.NativeWorkItemID, bool, bool, error) {
			return process(ctx, tracker.OrganizationID(tenant.Organization), bytes, dirtyOnly, cursor, attempted...)
		})
	}}
}
