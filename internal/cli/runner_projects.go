package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"time"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/project"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type runnerProjects struct {
	mu          sync.Mutex
	client      *hubclient.Client
	cfg         globalconfig.Config
	setup       *project.RunnerSetup
	logger      *slog.Logger
	checks      map[string]runnerauth.LocalChecks
	lastRefresh time.Time
	apply       func(context.Context, globalconfig.Config) error
	runtime     func() globalconfig.Config
	checkout    func(context.Context, globalconfig.Project, string) error
}

func (p *runnerProjects) prepare(ctx context.Context, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncRuntimeLocked()
	return p.prepareLocked(ctx, name)
}

func (p *runnerProjects) prepareLocked(ctx context.Context, name string) error {
	for _, selected := range project.ManagerConfigFromGlobal(p.cfg).Projects {
		if selected.ID != name {
			continue
		}
		if !runnerCheckoutReady(ctx, selected) {
			native, err := p.client.Native(tracker.OrganizationID(p.cfg.Client.OrganizationID), tracker.ProjectID(p.cfg.Client.NativeProjects[name]))
			if err != nil {
				return err
			}
			definition, err := native.Project(ctx)
			if err != nil {
				return err
			}
			clone := p.checkout
			if clone == nil {
				clone = prepareRunnerCheckout
			}
			if err := clone(ctx, selected, definition.CloneURL); err != nil {
				return err
			}
		}
		if id := p.cfg.Client.NativeProjects[name]; id != "" {
			selected.ID = id
		}
		return p.setup.Prepare(ctx, selected)
	}
	return fmt.Errorf("runner project checkout %s is unavailable", name)
}

func (p *runnerProjects) refresh(ctx context.Context, scheduler *hubclient.Scheduler) error {
	p.mu.Lock()
	locked := true
	defer func() {
		if locked {
			p.mu.Unlock()
		}
	}()
	if !p.lastRefresh.IsZero() && time.Now().Before(p.lastRefresh.Add(p.cfg.Client.HeartbeatInterval())) {
		return nil
	}
	p.lastRefresh = time.Now()
	if p.cfg.Client.IdentityFile == "" || len(p.cfg.Client.NativeProjects) == 0 {
		return nil
	}
	for _, id := range p.cfg.Client.NativeProjects {
		native, err := p.client.Native(tracker.OrganizationID(p.cfg.Client.OrganizationID), tracker.ProjectID(id))
		if err != nil {
			return err
		}
		supported, err := native.HubFeature(ctx, tracker.NativeProjectCheckoutCapability)
		if err != nil {
			return err
		}
		if !supported {
			return nil
		}
		break
	}
	identity, err := p.client.RunnerIdentity(ctx)
	if err != nil {
		return err
	}
	needsApply := p.syncRuntimeLocked()
	before := p.cfg
	p.cfg.Client.NativeProjects = maps.Clone(p.cfg.Client.NativeProjects)
	p.cfg.Projects = slices.Clone(p.cfg.Projects)
	root := filepath.Dir(p.cfg.Client.IdentityFile)
	if len(p.cfg.Projects) > 0 {
		root = filepath.Dir(p.cfg.Projects[0].Workdir)
	}
	for _, id := range identity.ProjectIDs {
		if slices.Contains(slices.Collect(maps.Values(p.cfg.Client.NativeProjects)), string(id)) {
			continue
		}
		native, err := p.client.Native(identity.OrganizationID, id)
		if err != nil {
			return err
		}
		definition, err := native.Project(ctx)
		if err != nil {
			p.logger.Warn("read assigned runner project failed", "project_id", id, "error", err)
			continue
		}
		name := runnerProjectSlug(definition.Name, id)
		if p.cfg.Client.NativeProjects[name] != "" || slices.ContainsFunc(p.cfg.Projects, func(selected globalconfig.Project) bool { return selected.ID == name }) {
			name = runnerProjectSlug(definition.Name+"-"+string(id), id)
		}
		workdir := filepath.Join(root, name)
		p.cfg.Client.NativeProjects[name] = string(id)
		p.cfg.Projects = append(p.cfg.Projects, globalconfig.Project{ID: name, Workdir: workdir, Workflow: filepath.Join(workdir, "WORKFLOW.md"), Weight: 1, Priority: 3})
	}
	changed := needsApply || !reflect.DeepEqual(before, p.cfg)
	for _, selected := range p.cfg.Projects {
		previous, reported := p.checks[selected.ID]
		setupErr := p.prepareLocked(ctx, selected.ID)
		checks := previous
		if setupErr == nil && (!reported || previous.Checkout != "passed" || previous.Setup != "passed") {
			checks = runnerProjectLocalChecks(ctx, p.cfg, selected.ID)
			native, err := p.client.Native(identity.OrganizationID, tracker.ProjectID(p.cfg.Client.NativeProjects[selected.ID]))
			if err != nil {
				return err
			}
			if checks.Checkout == "passed" {
				_, _, descriptor, err := resolveRunnerProjectPolicy(ctx, p.cfg, selected.ID)
				if err != nil {
					return err
				}
				if err := native.ReportObservedPolicy(ctx, descriptor); err != nil {
					return err
				}
			}
		}
		checks.Setup = "passed"
		if setupErr != nil {
			checks.Setup = "failed"
			if !reported {
				checks.Checkout, checks.Doctor, checks.Provider = "failed", "pending", "pending"
			}
			var checkoutErr *runnerCheckoutError
			if errors.As(setupErr, &checkoutErr) {
				checks.Checkout = "failed"
				checks.CheckoutMessage, checks.CheckoutFix = checkoutErr.message, checkoutErr.fix
			}
			p.logger.Warn("prepare assigned runner project failed", "project", selected.ID, "attribution", "instance", "error", setupErr)
		} else {
			checks.CheckoutMessage, checks.CheckoutFix = "", ""
		}
		changed = changed || !reflect.DeepEqual(previous, checks)
		p.checks[selected.ID] = checks
	}
	projects := make(map[string]tracker.ProjectID, len(p.cfg.Client.NativeProjects))
	for name, id := range p.cfg.Client.NativeProjects {
		projects[name] = tracker.ProjectID(id)
	}
	if err := scheduler.SetNativeProjects(identity.OrganizationID, projects, p.checks); err != nil {
		return err
	}
	current := p.cfg
	p.mu.Unlock()
	locked = false
	if changed && p.apply != nil {
		return p.apply(ctx, current)
	}
	return nil
}

func (p *runnerProjects) repository(ctx context.Context, name string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, selected := range p.cfg.Projects {
		if selected.ID == name {
			return runnerCheckoutRepository(ctx, selected)
		}
	}
	return ""
}

func (p *runnerProjects) syncRuntimeLocked() bool {
	if p.runtime == nil {
		return false
	}
	current := p.runtime()
	projects := slices.Clone(p.cfg.Projects)
	for i, selected := range projects {
		for _, next := range current.Projects {
			if next.ID == selected.ID {
				projects[i] = next
				break
			}
		}
	}
	current.Projects = projects
	current.Client.NativeProjects = p.cfg.Client.NativeProjects
	changed := !reflect.DeepEqual(p.runtime(), current)
	p.cfg = current
	return changed
}
