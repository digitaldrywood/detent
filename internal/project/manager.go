package project

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"reflect"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hub"
)

var (
	ErrManagerRunning  = errors.New("project manager already running")
	ErrProjectExists   = errors.New("project already exists")
	ErrProjectNotFound = errors.New("project not found")
)

const defaultMaxConcurrentStarts = 4

type Factory func(globalconfig.Project) (*Project, error)

type StartupConfig struct {
	Jitter              time.Duration
	MaxSpawnPerSecond   int
	MaxConcurrentStarts int
}

type ManagerConfig struct {
	Identity                 globalconfig.Identity
	Projects                 []globalconfig.Project
	Startup                  StartupConfig
	RuntimeCredentialVersion string
}

type ReconcileResult struct {
	Added     []ID
	Removed   []ID
	Changed   []ID
	Unchanged []ID
}

type startedProject struct {
	project *Project
}

type rollbackProject struct {
	project    *Project
	wasRunning bool
}

type ManagerDependencies struct {
	Registry            *Registry
	ProjectFactory      Factory
	ProjectDependencies Dependencies
	Events              *hub.Hub[Event]
	Logger              *slog.Logger
	Sleep               func(context.Context, time.Duration) error
	Jitter              func(time.Duration) time.Duration
}

type Manager struct {
	mu       sync.Mutex
	cfg      ManagerConfig
	registry *Registry
	factory  Factory
	sleep    func(context.Context, time.Duration) error
	jitter   func(time.Duration) time.Duration
	logger   *slog.Logger

	running bool
	spawned bool
}

func ManagerConfigFromGlobal(cfg globalconfig.Config) ManagerConfig {
	return normalizeManagerConfig(ManagerConfig{
		Identity: cfg.Global.Identity,
		Projects: cfg.Projects,
		Startup: StartupConfig{
			Jitter:              time.Duration(startupInt(cfg.Global.Startup, "jitter_seconds")) * time.Second,
			MaxSpawnPerSecond:   startupInt(cfg.Global.Startup, "max_spawn_per_second"),
			MaxConcurrentStarts: startupInt(cfg.Global.Startup, "max_concurrent_starts"),
		},
	})
}

func NewManager(cfg ManagerConfig, deps ManagerDependencies) (*Manager, error) {
	registry := deps.Registry
	if registry == nil {
		registry = NewRegistry()
	}

	projectDeps := deps.ProjectDependencies
	if projectDeps.Events == nil {
		projectDeps.Events = deps.Events
	}
	if projectDeps.Logger == nil {
		projectDeps.Logger = deps.Logger
	}

	factory := deps.ProjectFactory
	if factory == nil {
		factory = func(cfg globalconfig.Project) (*Project, error) {
			return Load(cfg, projectDeps)
		}
	}

	sleep := deps.Sleep
	if sleep == nil {
		sleep = sleepContext
	}

	jitter := deps.Jitter
	if jitter == nil {
		jitter = randomJitter
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}

	cfg = normalizeManagerConfig(cfg)
	return &Manager{
		cfg:      cfg,
		registry: registry,
		factory:  factory,
		sleep:    sleep,
		jitter:   jitter,
		logger:   logger,
	}, nil
}

func (m *Manager) Registry() *Registry {
	return m.registry
}

func (m *Manager) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	if m.running {
		m.mu.Unlock()
		return ErrManagerRunning
	}
	m.running = true

	projects := make([]*Project, 0, len(m.cfg.Projects))
	registered := make([]ID, 0, len(m.cfg.Projects))
	for _, cfg := range m.cfg.Projects {
		id, project, err := m.createProjectLocked(cfg)
		if err != nil {
			for _, registeredID := range registered {
				m.registry.Delete(registeredID)
			}
			m.running = false
			m.mu.Unlock()
			return errors.Join(err, closeProjectSlice(ctx, projects))
		}
		if _, ok := m.registry.Get(id); ok {
			for _, registeredID := range registered {
				m.registry.Delete(registeredID)
			}
			m.running = false
			m.mu.Unlock()
			return errors.Join(ErrProjectExists, project.close(ctx, false), closeProjectSlice(ctx, projects))
		}
		if err := m.registry.Set(project); err != nil {
			for _, registeredID := range registered {
				m.registry.Delete(registeredID)
			}
			m.running = false
			m.mu.Unlock()
			return errors.Join(err, project.close(ctx, false), closeProjectSlice(ctx, projects))
		}
		registered = append(registered, id)
		projects = append(projects, project)
	}
	startup := m.cfg.Startup
	m.mu.Unlock()

	started, err := m.startInitialProjects(ctx, projects, startup)
	if err != nil {
		return errors.Join(err, m.rollbackInitialStart(ctx, registered, projects, started))
	}
	if len(started) > 0 {
		m.mu.Lock()
		m.spawned = true
		m.mu.Unlock()
	}
	return nil
}

func (m *Manager) Add(ctx context.Context, cfg globalconfig.Project) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.addLocked(ctx, cfg)
}

func (m *Manager) Remove(ctx context.Context, id ID) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	return m.removeLocked(ctx, id)
}

func (m *Manager) Reconcile(ctx context.Context, cfg ManagerConfig) (ReconcileResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	cfg = normalizeManagerConfig(cfg)
	desired := make(map[ID]globalconfig.Project, len(cfg.Projects))
	for i, project := range cfg.Projects {
		normalized := project
		id := ID(normalized.ID)
		if id == "" {
			return ReconcileResult{}, ErrMissingProjectID
		}
		if _, ok := desired[id]; ok {
			return ReconcileResult{}, ErrProjectExists
		}
		cfg.Projects[i] = normalized
		desired[id] = normalized
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	runtimeCredentialChanged := m.cfg.RuntimeCredentialVersion != cfg.RuntimeCredentialVersion
	result := ReconcileResult{}
	for _, current := range m.registry.List() {
		id := current.ID()
		next, ok := desired[id]
		if !ok {
			result.Removed = append(result.Removed, id)
			continue
		}
		if !runtimeCredentialChanged && sameProjectConfig(current.Config(), next) {
			result.Unchanged = append(result.Unchanged, id)
			continue
		}
		result.Changed = append(result.Changed, id)
	}

	for _, next := range cfg.Projects {
		id := ID(next.ID)
		if _, ok := m.registry.Get(id); !ok {
			result.Added = append(result.Added, id)
		}
	}

	prepared := make(map[ID]*Project, len(result.Added)+len(result.Changed))
	for _, id := range result.Changed {
		_, preparedProject, err := m.createProjectLocked(desired[id])
		if err != nil {
			return result, errors.Join(err, closePreparedProjects(ctx, prepared))
		}
		prepared[id] = preparedProject
	}
	for _, id := range result.Added {
		_, preparedProject, err := m.createProjectLocked(desired[id])
		if err != nil {
			return result, errors.Join(err, closePreparedProjects(ctx, prepared))
		}
		prepared[id] = preparedProject
	}

	previous := m.cfg
	previousSpawned := m.spawned
	m.cfg.Startup = cfg.Startup
	stopped := make([]rollbackProject, 0, len(result.Removed)+len(result.Changed))
	started := make([]startedProject, 0, len(prepared))
	added := map[ID]struct{}{}
	rollback := func() error {
		cleanupErr := m.stopUncommittedStartedProjects(ctx, started)
		cleanupErr = errors.Join(cleanupErr, closePreparedProjects(ctx, prepared))
		for id := range added {
			m.registry.Delete(id)
		}
		for i := len(stopped) - 1; i >= 0; i-- {
			item := stopped[i]
			if err := m.registry.Set(item.project); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		m.cfg = previous
		m.spawned = previousSpawned
		for i := len(stopped) - 1; i >= 0; i-- {
			item := stopped[i]
			if !item.wasRunning || item.project.Running() {
				continue
			}
			if err := m.restartProjectLocked(ctx, item.project, false); err != nil {
				cleanupErr = errors.Join(cleanupErr, err)
			}
		}
		return cleanupErr
	}

	for _, id := range result.Removed {
		current, ok := m.registry.Get(id)
		if !ok || current == nil {
			return result, errors.Join(ErrProjectNotFound, rollback())
		}
		wasRunning := current.Running()
		if wasRunning {
			if err := current.stop(ctx, false); err != nil {
				return result, errors.Join(err, rollback())
			}
		}
		stopped = append(stopped, rollbackProject{project: current, wasRunning: wasRunning})
		if !m.registry.Delete(id) {
			return result, errors.Join(ErrProjectNotFound, rollback())
		}
	}
	for _, id := range result.Changed {
		current, ok := m.registry.Get(id)
		if !ok || current == nil {
			return result, errors.Join(ErrProjectNotFound, rollback())
		}
		wasRunning := current.Running()
		if wasRunning {
			if err := current.stop(ctx, false); err != nil {
				return result, errors.Join(err, rollback())
			}
		}
		stopped = append(stopped, rollbackProject{project: current, wasRunning: wasRunning})
		preparedProject, err := preparedProjectByID(prepared, id)
		if err != nil {
			return result, errors.Join(err, rollback())
		}
		didStart, err := m.startPreparedProjectLocked(ctx, preparedProject)
		if err != nil {
			return result, errors.Join(err, rollback())
		}
		if didStart {
			started = append(started, startedProject{project: preparedProject})
		}
		if err := m.registry.Set(preparedProject); err != nil {
			return result, errors.Join(err, rollback())
		}
	}
	for _, id := range result.Added {
		preparedProject, err := preparedProjectByID(prepared, id)
		if err != nil {
			return result, errors.Join(err, rollback())
		}
		didStart, err := m.startPreparedProjectLocked(ctx, preparedProject)
		if err != nil {
			if len(result.Removed) > 0 || len(result.Changed) > 0 {
				return result, errors.Join(err, rollback())
			}
			m.logProjectStartupFailure(id, err)
			if err := m.registry.Set(preparedProject); err != nil {
				return result, errors.Join(err, rollback())
			}
			added[id] = struct{}{}
			continue
		}
		if didStart {
			started = append(started, startedProject{project: preparedProject})
		}
		if err := m.registry.Set(preparedProject); err != nil {
			return result, errors.Join(err, rollback())
		}
		added[id] = struct{}{}
	}

	m.cfg = cfg
	for _, item := range stopped {
		if item.wasRunning {
			item.project.publishStopped(nil)
		}
	}
	for _, item := range started {
		item.project.publishStarted()
	}
	return result, closeStoppedProjects(ctx, stopped)
}

func (m *Manager) removeLocked(ctx context.Context, id ID) error {
	project, ok := m.registry.Get(id)
	if !ok || project == nil {
		return ErrProjectNotFound
	}
	if err := project.close(ctx, true); err != nil {
		return err
	}
	m.registry.Delete(id)
	return nil
}

func (m *Manager) Pause(ctx context.Context, id ID) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	project, ok := m.registry.Get(id)
	if !ok || project == nil {
		return ErrProjectNotFound
	}
	return project.Pause(ctx)
}

func (m *Manager) Unpause(ctx context.Context, id ID) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	project, ok := m.registry.Get(id)
	if !ok || project == nil {
		return ErrProjectNotFound
	}
	if !project.Paused() {
		return nil
	}
	if m.running {
		if err := m.waitBeforeSpawn(ctx); err != nil {
			return err
		}
	}
	if err := project.Unpause(ctx); err != nil {
		return err
	}
	m.spawned = true
	return nil
}

func (m *Manager) addLocked(ctx context.Context, cfg globalconfig.Project) error {
	cfg = normalizeManagerProjectConfigWithIdentity(cfg, m.cfg.Identity)
	id := ID(cfg.ID)
	if id == "" {
		return ErrMissingProjectID
	}
	if _, ok := m.registry.Get(id); ok {
		return ErrProjectExists
	}

	_, project, err := m.createProjectLocked(cfg)
	if err != nil {
		return err
	}
	return m.registerProjectLocked(ctx, id, project)
}

func (m *Manager) createProjectLocked(cfg globalconfig.Project) (ID, *Project, error) {
	id := normalizeProjectID(ID(cfg.ID))
	if id == "" {
		return "", nil, ErrMissingProjectID
	}
	cfg.ID = string(id)
	project, err := m.factory(cfg)
	if err != nil {
		return "", nil, fmt.Errorf("create project %s: %w", id, err)
	}
	if project == nil {
		return "", nil, ErrMissingProject
	}
	return id, project, nil
}

func (m *Manager) registerProjectLocked(ctx context.Context, id ID, project *Project) error {
	if project == nil {
		return ErrMissingProject
	}
	if err := m.registry.Set(project); err != nil {
		return err
	}
	if !m.running || project.Paused() {
		return nil
	}
	if err := m.startLocked(ctx, project); err != nil {
		m.registry.Delete(id)
		return errors.Join(err, project.close(ctx, false))
	}
	return nil
}

func (m *Manager) startPreparedProjectLocked(ctx context.Context, project *Project) (bool, error) {
	if project == nil {
		return false, ErrMissingProject
	}
	if !m.running || project.Paused() {
		return false, nil
	}
	if err := m.startProjectLocked(ctx, project, false); err != nil {
		return false, err
	}
	return true, nil
}

func (m *Manager) startLocked(ctx context.Context, project *Project) error {
	return m.startProjectLocked(ctx, project, true)
}

func (m *Manager) startProjectLocked(ctx context.Context, project *Project, publishEvents bool) error {
	if project == nil {
		return ErrMissingProject
	}
	if err := m.waitBeforeSpawn(ctx); err != nil {
		return err
	}
	if err := project.start(ctx, startOptions{provision: true, publishEvents: publishEvents}); err != nil {
		return err
	}
	m.spawned = true
	return nil
}

func (m *Manager) restartProjectLocked(ctx context.Context, project *Project, publishEvents bool) error {
	if err := m.waitBeforeSpawn(ctx); err != nil {
		return err
	}
	if err := project.restart(ctx, startOptions{provision: true, publishEvents: publishEvents}); err != nil {
		return err
	}
	m.spawned = true
	return nil
}

func (m *Manager) stopUncommittedStartedProjects(ctx context.Context, started []startedProject) error {
	var cleanupErr error
	for i := len(started) - 1; i >= 0; i-- {
		item := started[i]
		if item.project.Running() {
			cleanupErr = errors.Join(cleanupErr, item.project.stop(ctx, false))
		}
	}
	return cleanupErr
}

func (m *Manager) startInitialProjects(
	ctx context.Context,
	projects []*Project,
	startup StartupConfig,
) ([]startedProject, error) {
	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(maxConcurrentStarts(startup, len(projects)))

	limiter := startupLimiter{
		startup: startup,
		sleep:   m.sleep,
		jitter:  m.jitter,
	}
	var startedMu sync.Mutex
	started := make([]startedProject, 0, len(projects))
	for _, project := range projects {
		trackedProject := project
		if trackedProject.Paused() {
			continue
		}
		group.Go(func() error {
			if err := limiter.wait(groupCtx); err != nil {
				return err
			}
			if err := trackedProject.provision(groupCtx); err != nil {
				return err
			}
			if err := groupCtx.Err(); err != nil {
				return err
			}
			if err := trackedProject.start(ctx, startOptions{provision: false, publishEvents: true}); err != nil {
				return err
			}
			startedMu.Lock()
			started = append(started, startedProject{project: trackedProject})
			startedMu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return started, err
	}
	return started, nil
}

func (m *Manager) rollbackInitialStart(
	ctx context.Context,
	registered []ID,
	projects []*Project,
	started []startedProject,
) error {
	cleanupErr := m.stopUncommittedStartedProjects(ctx, started)
	cleanupErr = errors.Join(cleanupErr, closeProjectSlice(ctx, projects))

	m.mu.Lock()
	defer m.mu.Unlock()

	for _, id := range registered {
		m.registry.Delete(id)
	}
	m.running = false
	m.spawned = false
	return cleanupErr
}

type startupLimiter struct {
	startup StartupConfig
	sleep   func(context.Context, time.Duration) error
	jitter  func(time.Duration) time.Duration

	mu      sync.Mutex
	spawned bool
}

func (l *startupLimiter) wait(ctx context.Context) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	delay := spawnDelay(l.startup, l.spawned, l.jitter)
	if delay > 0 {
		if err := l.sleep(ctx, delay); err != nil {
			return err
		}
	}
	l.spawned = true
	return nil
}

func maxConcurrentStarts(startup StartupConfig, projectCount int) int {
	limit := startup.MaxConcurrentStarts
	if limit <= 0 {
		limit = defaultMaxConcurrentStarts
	}
	if projectCount > 0 && limit > projectCount {
		return projectCount
	}
	return limit
}

func closeProjectSlice(ctx context.Context, projects []*Project) error {
	var cleanupErr error
	for _, project := range projects {
		if project == nil {
			continue
		}
		cleanupErr = errors.Join(cleanupErr, project.close(ctx, false))
	}
	return cleanupErr
}

func closePreparedProjects(ctx context.Context, prepared map[ID]*Project) error {
	var cleanupErr error
	for _, project := range prepared {
		if project == nil {
			continue
		}
		cleanupErr = errors.Join(cleanupErr, project.close(ctx, false))
	}
	return cleanupErr
}

func closeStoppedProjects(ctx context.Context, stopped []rollbackProject) error {
	var cleanupErr error
	for _, item := range stopped {
		if item.project == nil {
			continue
		}
		cleanupErr = errors.Join(cleanupErr, item.project.close(ctx, false))
	}
	return cleanupErr
}

func preparedProjectByID(prepared map[ID]*Project, id ID) (*Project, error) {
	project := prepared[id]
	if project == nil {
		return nil, ErrMissingProject
	}
	return project, nil
}

func (m *Manager) waitBeforeSpawn(ctx context.Context) error {
	delay := spawnDelay(m.cfg.Startup, m.spawned, m.jitter)
	if delay <= 0 {
		return nil
	}
	return m.sleep(ctx, delay)
}

func (m *Manager) logProjectStartupFailure(id ID, err error) {
	logger := m.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.Warn("project startup failed", "project_id", id, "error", err)
}

func spawnDelay(startup StartupConfig, spawned bool, jitter func(time.Duration) time.Duration) time.Duration {
	if !spawned {
		return 0
	}
	delay := time.Duration(0)
	if startup.MaxSpawnPerSecond > 0 {
		delay += time.Second / time.Duration(startup.MaxSpawnPerSecond)
	}
	if startup.Jitter > 0 && jitter != nil {
		delay += jitter(startup.Jitter)
	}
	return delay
}

func startupInt(values map[string]any, key string) int {
	value, ok := values[key]
	if !ok {
		return 0
	}
	number, ok := value.(int)
	if !ok || number <= 0 {
		return 0
	}
	return number
}

func normalizeManagerProjectConfig(cfg globalconfig.Project) globalconfig.Project {
	cfg.ID = string(normalizeProjectID(ID(cfg.ID)))
	cfg.Identity.Normalize()
	return cfg
}

func normalizeManagerProjectConfigWithIdentity(
	cfg globalconfig.Project,
	identity globalconfig.Identity,
) globalconfig.Project {
	cfg = normalizeManagerProjectConfig(cfg)
	identity.Normalize()
	if identity.Configured() {
		cfg.Identity = identity
	}
	return cfg
}

func normalizeManagerConfig(cfg ManagerConfig) ManagerConfig {
	cfg.Identity.Normalize()
	cfg.Projects = append([]globalconfig.Project(nil), cfg.Projects...)
	for i := range cfg.Projects {
		cfg.Projects[i] = normalizeManagerProjectConfigWithIdentity(cfg.Projects[i], cfg.Identity)
	}
	return cfg
}

func sameProjectConfig(left globalconfig.Project, right globalconfig.Project) bool {
	return reflect.DeepEqual(normalizeManagerProjectConfig(left), normalizeManagerProjectConfig(right))
}

func sleepContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func randomJitter(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}

	value, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return 0
	}
	return time.Duration(value.Int64())
}
