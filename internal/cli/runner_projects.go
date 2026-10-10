package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
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
	if p.cfg.Client.IdentityFile == "" {
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
	p.cfg, err = runnerProjectsForIdentity(ctx, p.cfg, p.client, identity, false)
	if err != nil {
		return err
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

func (p *runnerProjects) runnerSetupDeclared(ctx context.Context, name string) *bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncRuntimeLocked()
	for _, selected := range project.ManagerConfigFromGlobal(p.cfg).Projects {
		if selected.ID != name {
			continue
		}
		if id := p.cfg.Client.NativeProjects[name]; id != "" {
			selected.ID = id
		}
		workflow, err := project.LoadWorkflowContext(ctx, selected)
		if err != nil {
			return nil
		}
		return new(workflow.Config.Hooks.RunnerSetup != "")
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

func resolveRunnerProjects(ctx context.Context, cfg globalconfig.Config) (globalconfig.Config, error) {
	if cfg.Client.IdentityFile == "" || cfg.WorkspaceRoot == "" {
		return cfg, nil
	}
	settings := cfg.Client.Normalized()
	client, err := hubclient.New(hubclient.Config{URL: settings.URL, IdentityFile: settings.IdentityFile, HTTPClient: &http.Client{Timeout: settings.RequestTimeout()}})
	if err != nil {
		return cfg, err
	}
	identity, err := client.RunnerIdentity(ctx)
	if err != nil {
		return cfg, fmt.Errorf("read runner's allowed projects: %w", err)
	}
	return runnerProjectsForIdentity(ctx, cfg, client, identity, true)
}

func runnerProjectsForIdentity(ctx context.Context, cfg globalconfig.Config, client *hubclient.Client, identity runnerauth.Identity, prepare bool) (globalconfig.Config, error) {
	if string(identity.OrganizationID) != cfg.Client.OrganizationID {
		return cfg, errors.New("runner organization does not match enrolled identity")
	}
	previous := cfg.Projects
	cfg.Projects = []globalconfig.Project{}
	cfg.Client.NativeProjects = map[string]string{}
	ids := slices.Clone(identity.ProjectIDs)
	slices.Sort(ids)
	for _, id := range ids {
		name := string(id)
		cfg.Client.NativeProjects[name] = string(id)
		native, err := client.Native(identity.OrganizationID, id)
		if err != nil {
			return cfg, err
		}
		remote, err := native.Project(ctx)
		if err != nil {
			slog.Error("runner project setup failed", "runner_id", identity.RunnerID, "project_id", id, "attribution", "instance", "error", err)
			continue
		}
		if remote.ID != id || remote.OrganizationID != identity.OrganizationID {
			return cfg, errors.New("hub returned an unexpected project identity")
		}
		repository := remote.Repository
		if repository == "" {
			repository, _ = doctorGitHubRepositoryFromRemoteURL(remote.CloneURL)
		}
		locations := slices.Concat(cfg.RunnerCheckouts, previous)
		workdir, retainedName, err := runnerProjectCheckout(ctx, cfg.WorkspaceRoot, name, repository, locations)
		if err != nil {
			slog.Error("runner project setup failed", "runner_id", identity.RunnerID, "project_id", id, "attribution", "instance", "error", err)
			continue
		}
		if retainedName != "" && cfg.Client.NativeProjects[retainedName] == "" {
			delete(cfg.Client.NativeProjects, name)
			name = retainedName
			cfg.Client.NativeProjects[name] = string(id)
		}
		selected := globalconfig.Project{ID: name, Workflow: filepath.Join(workdir, "WORKFLOW.md"), WorkflowRef: "origin/HEAD", Workdir: workdir, Weight: 1, GlobalCache: cfg.Global.Cache.Normalized()}
		if configured, ok := configuredRunnerWorkflow(locations, name, workdir); ok {
			selected.Workflow, selected.WorkflowRef = configured.Workflow, configured.WorkflowRef
		}
		if prepare {
			cloneURL := remote.CloneURL
			if cloneURL == "" && repository != "" {
				cloneURL = "https://github.com/" + repository + ".git"
			}
			if err := prepareRunnerCheckout(ctx, selected, cloneURL); err != nil {
				slog.Error("runner project setup failed", "runner_id", identity.RunnerID, "project_id", id, "attribution", "instance", "error", err)
				continue
			}
		}
		cfg.Projects = append(cfg.Projects, selected)
	}
	return cfg, ctx.Err()
}

func runnerProjectCheckout(ctx context.Context, root, name, repository string, retained []globalconfig.Project) (string, string, error) {
	target := filepath.Join(root, name)
	entries, err := os.ReadDir(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", "", err
	}
	candidates := make(map[string]string)
	for _, entry := range entries {
		candidates[canonicalCheckoutPath(filepath.Join(root, entry.Name()))] = entry.Name()
	}
	for _, selected := range retained {
		candidates[canonicalCheckoutPath(selected.Workdir)] = selected.ID
	}
	var matches []string
	for candidate := range candidates {
		if _, err := os.Stat(filepath.Join(candidate, ".git")); err != nil {
			continue
		}
		cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
		cmd.Dir = candidate
		remote, err := cmd.Output()
		if err != nil {
			continue
		}
		linked, _ := doctorGitHubRepositoryFromRemoteURL(strings.TrimSpace(string(remote)))
		if repository != "" && strings.EqualFold(linked, repository) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) > 1 {
		return "", "", fmt.Errorf("multiple runner checkouts for %s require selecting the retained source", repository)
	}
	if len(matches) == 1 {
		return matches[0], candidates[matches[0]], nil
	}
	if _, err := os.Stat(filepath.Join(target, ".git")); err == nil {
		return "", "", fmt.Errorf("runner checkout %s does not match Cloud repository %s", target, repository)
	}
	return target, "", nil
}

func canonicalCheckoutPath(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return filepath.Clean(path)
}

func configuredRunnerWorkflow(locations []globalconfig.Project, name, workdir string) (globalconfig.Project, bool) {
	for _, location := range locations {
		if location.ID == name && location.Workflow != "" && canonicalCheckoutPath(location.Workdir) == canonicalCheckoutPath(workdir) {
			return location, true
		}
	}
	return globalconfig.Project{}, false
}
