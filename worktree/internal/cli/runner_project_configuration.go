package cli

import (
	"context"
	"sync"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func runnerProjectConfigurationOwner(selected globalconfig.Config, runtime func() globalconfig.Config, owner *project.ConfigurationOwner) func(context.Context, string, *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration {
	enrolled, enrolledErr := runnerauth.Load(selected.Client.IdentityFile)
	var mu sync.Mutex
	receipts := map[string]runnerauth.ProjectConfiguration{}
	return func(ctx context.Context, id string, request *runnerauth.ProjectConfigurationRequest) runnerauth.ProjectConfiguration {
		mu.Lock()
		defer mu.Unlock()
		view := project.MissingConfigurationOwner(id)
		if enrolledErr != nil || runtime == nil {
			return view
		}
		current := runtime()
		if observeRunnerCapacityConfiguration(selected, current, enrolled) == nil {
			return view
		}
		identity, err := runnerauth.Load(selected.Client.IdentityFile)
		if err != nil || !(runnerauth.Routing{Scope: identity.Identity.Scope, ProjectIDs: identity.Identity.ProjectIDs}).AllowsProject(tracker.ProjectID(id)) || !identity.Identity.ExpiresAt.After(time.Now()) {
			return view
		}
		var local string
		for name, native := range current.Client.NativeProjects {
			if native == id && selected.Client.NativeProjects[name] == native {
				if local != "" {
					return view
				}
				local = name
			}
		}
		if local == "" {
			return view
		}
		view = owner.Read(ctx, local)
		if request != nil {
			if request.ProjectID != id || request.RequestID == "" {
				return project.MissingConfigurationOwner(id)
			}
			if previous, ok := receipts[id]; ok && previous.RequestID == request.RequestID {
				return previous
			}
			translated := *request
			translated.ProjectID = local
			view = owner.Apply(ctx, request.Operation, translated)
			view.RequestID = request.RequestID
			view.ProjectID = id
			receipts[id] = view
		}
		view.ProjectID = id
		if request == nil {
			if previous, ok := receipts[id]; ok && previous.ConfigRevision == view.ConfigRevision {
				view.RequestID, view.Saved, view.Applied = previous.RequestID, previous.Saved, previous.Applied
				if previous.EffectivePolicy == nil || view.EffectivePolicy == nil || previous.EffectivePolicy.ID != view.EffectivePolicy.ID {
					view.Applied = false
				}
			}
		}
		return view
	}
}
