package cli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
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
	"github.com/digitaldrywood/detent/internal/policy"
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
	problems    map[string]runnerauth.Problem
	lastRefresh time.Time
	apply       func(context.Context, globalconfig.Config) error
	runtime     func() globalconfig.Config
	checkout    func(context.Context, globalconfig.Project, string) error
	preparing   map[string]*runnerProjectPreparation
	observed    map[string]time.Time
}

const runnerReadinessRecheck = 30 * time.Second

func runnerProjectObservationDue(previous runnerauth.LocalChecks, reported bool, observedAt, now time.Time) (prepare, observe bool) {
	if !reported || previous.Checkout != "passed" || previous.Setup != "passed" {
		return true, false
	}
	return false, !previous.WorkReady() && now.Sub(observedAt) >= runnerReadinessRecheck
}

type runnerProjectPreparation struct {
	selected globalconfig.Project
	cancel   context.CancelFunc
	done     chan struct{}
	checks   runnerauth.LocalChecks
	err      error
}

func (p *runnerProjects) prepare(ctx context.Context, name string) error {
	p.mu.Lock()
	p.syncRuntimeLocked()
	cfg := p.cfg
	p.mu.Unlock()
	return p.prepareConfiguration(ctx, cfg, name)
}

func (p *runnerProjects) providerConfiguration() globalconfig.Config {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.syncRuntimeLocked()
	return p.cfg
}

func (p *runnerProjects) reportedProblems() []runnerauth.Problem {
	p.mu.Lock()
	defer p.mu.Unlock()
	problems := make([]runnerauth.Problem, 0, len(p.problems))
	for _, problem := range p.problems {
		problems = append(problems, problem)
	}
	return problems
}

func (p *runnerProjects) prepareConfiguration(ctx context.Context, cfg globalconfig.Config, name string) error {
	for _, selected := range project.ManagerConfigFromGlobal(cfg).Projects {
		if selected.ID != name {
			continue
		}
		if !runnerCheckoutReady(ctx, selected) {
			native, err := p.client.Native(tracker.OrganizationID(cfg.Client.OrganizationID), tracker.ProjectID(cfg.Client.NativeProjects[name]))
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
			cloneURL := definition.CloneURL
			if cloneURL == "" && definition.Repository != "" {
				cloneURL = "https://github.com/" + definition.Repository + ".git"
			}
			if err := clone(ctx, selected, cloneURL); err != nil {
				return err
			}
		}
		if id := cfg.Client.NativeProjects[name]; id != "" {
			selected.ID = id
		}
		return p.setup.Prepare(ctx, selected)
	}
	return fmt.Errorf("runner project checkout %s is unavailable", name)
}

func (p *runnerProjects) startPreparation(ctx context.Context, cfg globalconfig.Config, selected globalconfig.Project, observeOnly bool) *runnerProjectPreparation {
	ctx, cancel := context.WithCancel(ctx)
	run := &runnerProjectPreparation{selected: selected, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(run.done)
		defer cancel()
		if observeOnly {
			run.checks = runnerProjectLocalChecks(ctx, cfg, selected.ID)
			run.checks.Setup = "passed"
			return
		}
		run.err = p.prepareConfiguration(ctx, cfg, selected.ID)
		if run.err != nil {
			run.checks = runnerauth.LocalChecks{Checkout: "failed", Setup: "failed", Doctor: "pending", Provider: "pending"}
			var checkoutErr *runnerCheckoutError
			if errors.As(run.err, &checkoutErr) {
				run.checks.CheckoutMessage, run.checks.CheckoutFix = checkoutErr.guidance(), checkoutErr.fix
			}
			return
		}
		run.checks = runnerProjectLocalChecks(ctx, cfg, selected.ID)
		run.checks.Setup = "passed"
		native, err := p.client.Native(tracker.OrganizationID(cfg.Client.OrganizationID), tracker.ProjectID(cfg.Client.NativeProjects[selected.ID]))
		if err == nil && run.checks.Checkout == "passed" {
			var descriptor policy.Descriptor
			_, _, descriptor, err = resolveRunnerProjectPolicy(ctx, cfg, selected.ID)
			if err == nil {
				err = native.ReportObservedPolicy(ctx, descriptor)
			}
		}
		if err != nil {
			run.err = err
			run.checks.Setup = "failed"
		}
	}()
	return run
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
	p.cfg, err = runnerProjectsForIdentity(ctx, p.cfg, p.client, identity, false, false)
	if err != nil {
		return err
	}
	changed := needsApply || !reflect.DeepEqual(before, p.cfg)
	if p.preparing == nil {
		p.preparing = make(map[string]*runnerProjectPreparation)
	}
	for name, run := range p.preparing {
		if !slices.ContainsFunc(p.cfg.Projects, func(selected globalconfig.Project) bool { return reflect.DeepEqual(selected, run.selected) }) {
			run.cancel()
			select {
			case <-run.done:
				delete(p.preparing, name)
			default:
			}
			delete(p.checks, name)
			delete(p.problems, name)
			delete(p.observed, name)
		}
	}
	for name := range p.checks {
		if _, allowed := p.cfg.Client.NativeProjects[name]; !allowed {
			delete(p.checks, name)
			delete(p.problems, name)
			delete(p.observed, name)
		}
	}
	for _, selected := range p.cfg.Projects {
		previous, reported := p.checks[selected.ID]
		run := p.preparing[selected.ID]
		if run != nil && !reflect.DeepEqual(run.selected, selected) {
			continue
		}
		if run == nil {
			prepare, observe := runnerProjectObservationDue(previous, reported, p.observed[selected.ID], time.Now())
			if !prepare && !observe {
				continue
			}
			run = p.startPreparation(ctx, p.cfg, selected, observe)
			p.preparing[selected.ID] = run
			if !reported {
				p.checks[selected.ID] = runnerauth.LocalChecks{Checkout: "pending", Setup: "pending", Doctor: "pending", Provider: "pending"}
			}
			continue
		}
		select {
		case <-run.done:
			run.cancel()
			delete(p.preparing, selected.ID)
			if p.observed == nil {
				p.observed = make(map[string]time.Time)
			}
			p.observed[selected.ID] = time.Now()
			p.checks[selected.ID] = run.checks
			delete(p.problems, selected.ID)
			changed = changed || !reflect.DeepEqual(previous, run.checks)
			if run.err != nil {
				problem := runnerauth.NewProblem("settings_invalid")
				problem.ProjectID = p.cfg.Client.NativeProjects[selected.ID]
				problem.Subject = "checkout"
				problem.ErrorOutput = run.err.Error()
				problem.ReportedAt = time.Now()
				if run.checks.CheckoutMessage != "" {
					problem.Message, problem.FixHint = run.checks.CheckoutMessage, run.checks.CheckoutFix
				}
				if p.problems == nil {
					p.problems = make(map[string]runnerauth.Problem)
				}
				p.problems[selected.ID] = runnerauth.SanitizeProblem(problem)
				p.logger.Warn("prepare assigned runner project failed", "project", selected.ID, "attribution", "instance", "error", run.err)
			}
		default:
		}
	}
	projects := make(map[string]tracker.ProjectID, len(p.cfg.Client.NativeProjects))
	for name, id := range p.cfg.Client.NativeProjects {
		projects[name] = tracker.ProjectID(id)
	}
	if err := scheduler.SetNativeProjects(identity.OrganizationID, projects, p.checks); err != nil {
		return err
	}
	current := p.cfg
	current.Projects = slices.DeleteFunc(slices.Clone(current.Projects), func(selected globalconfig.Project) bool {
		return p.checks[selected.ID].Setup != "passed"
	})
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

func resolveRunnerProjects(ctx context.Context, cfg globalconfig.Config, previous ...globalconfig.Config) (globalconfig.Config, error) {
	return resolveRunnerProjectsWithMode(ctx, cfg, false, previous...)
}

func resolveRunnerProjectsReadOnly(ctx context.Context, cfg globalconfig.Config) (globalconfig.Config, error) {
	return resolveRunnerProjectsWithMode(ctx, cfg, true)
}

func resolveRunnerProjectsWithMode(ctx context.Context, cfg globalconfig.Config, readOnly bool, previous ...globalconfig.Config) (globalconfig.Config, error) {
	if cfg.Client.IdentityFile == "" || cfg.WorkspaceRoot == "" {
		return cfg, nil
	}
	if len(previous) > 0 {
		current := previous[0]
		if cfg.Client.IdentityFile == current.Client.IdentityFile && cfg.Client.URL == current.Client.URL &&
			cfg.Client.OrganizationID == current.Client.OrganizationID && cfg.WorkspaceRoot == current.WorkspaceRoot {
			cfg.Projects = slices.Clone(current.Projects)
			cfg.Client.NativeProjects = maps.Clone(current.Client.NativeProjects)
		}
	}
	settings := cfg.Client.Normalized()
	newClient := hubclient.New
	if readOnly {
		newClient = hubclient.NewRunnerReader
	}
	client, err := newClient(hubclient.Config{URL: settings.URL, IdentityFile: settings.IdentityFile, HTTPClient: &http.Client{Timeout: settings.RequestTimeout()}})
	if err != nil {
		return cfg, err
	}
	identity, err := client.RunnerIdentity(ctx)
	if err != nil {
		return cfg, fmt.Errorf("read runner's allowed projects: %w", err)
	}
	resolved, err := runnerProjectsForIdentity(ctx, cfg, client, identity, !readOnly && len(previous) == 0, readOnly)
	if len(previous) > 0 && err == nil {
		resolved.Projects = slices.DeleteFunc(resolved.Projects, func(selected globalconfig.Project) bool {
			return !slices.ContainsFunc(previous[0].Projects, func(current globalconfig.Project) bool { return current.ID == selected.ID }) && !runnerCheckoutReady(ctx, selected)
		})
	}
	return resolved, err
}

func runnerProjectsForIdentity(ctx context.Context, cfg globalconfig.Config, client *hubclient.Client, identity runnerauth.Identity, prepare, readOnly bool) (globalconfig.Config, error) {
	if string(identity.OrganizationID) != cfg.Client.OrganizationID {
		return cfg, errors.New("runner organization does not match enrolled identity")
	}
	previous := cfg.Projects
	previousNative := cfg.Client.NativeProjects
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
			if readOnly {
				var apiErr *hubclient.APIError
				authorityFailure := !errors.Is(err, hubclient.ErrUnavailable)
				if errors.As(err, &apiErr) {
					authorityFailure = apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden
				}
				if authorityFailure {
					return cfg, fmt.Errorf("read runner project: %w", err)
				}
			}
			slog.Error("runner project setup failed", "runner_id", identity.RunnerID, "project_id", id, "attribution", "instance", "error", err)
			if kept, ok := retainedRunnerProject(previous, previousNative, id, err); ok {
				delete(cfg.Client.NativeProjects, name)
				cfg.Client.NativeProjects[kept.ID] = string(id)
				cfg.Projects = append(cfg.Projects, kept)
			}
			continue
		}
		if remote.ID != id || remote.OrganizationID != identity.OrganizationID {
			return cfg, errors.New("hub returned an unexpected project identity")
		}
		repository := remote.Repository
		if repository == "" {
			repository, _ = doctorGitHubRepositoryFromRemoteURL(remote.CloneURL)
		}
		if repository == "" && remote.CloneURL == "" {
			slog.Info("Waiting for a repository: bind one in project setup", "runner_id", identity.RunnerID, "project_id", id)
			continue
		}
		locations := previous
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

func retainedRunnerProject(previous []globalconfig.Project, previousNative map[string]string, id tracker.ProjectID, err error) (globalconfig.Project, bool) {
	if !errors.Is(err, hubclient.ErrUnavailable) {
		return globalconfig.Project{}, false
	}
	for _, project := range previous {
		if previousNative[project.ID] == string(id) {
			return project, true
		}
	}
	return globalconfig.Project{}, false
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
