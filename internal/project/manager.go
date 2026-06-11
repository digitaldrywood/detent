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

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/hub"
)

var (
	ErrManagerRunning  = errors.New("project manager already running")
	ErrProjectExists   = errors.New("project already exists")
	ErrProjectNotFound = errors.New("project not found")
)

type Factory func(globalconfig.Project) (*Project, error)

type StartupConfig struct {
	Jitter            time.Duration
	MaxSpawnPerSecond int
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

	running bool
	spawned bool
}

func ManagerConfigFromGlobal(cfg globalconfig.Config) ManagerConfig {
	return normalizeManagerConfig(ManagerConfig{
		Identity: cfg.Global.Identity,
		Projects: cfg.Projects,
		Startup: StartupConfig{
			Jitter:            time.Duration(startupInt(cfg.Global.Startup, "jitter_seconds")) * time.Second,
			MaxSpawnPerSecond: startupInt(cfg.Global.Startup, "max_spawn_per_second"),
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

	cfg = normalizeManagerConfig(cfg)
	return &Manager{
		cfg:      cfg,
		registry: registry,
		factory:  factory,
		sleep:    sleep,
		jitter:   jitter,
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
	defer m.mu.Unlock()

	if m.running {
		return ErrManagerRunning
	}
	m.running = true

	for _, cfg := range m.cfg.Projects {
		if err := m.addLocked(ctx, cfg); err != nil {
			m.running = false
			return err
		}
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
		if !ok {
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
		if !ok {
			return result, errors.Join(ErrProjectNotFound, rollback())
		}
		wasRunning := current.Running()
		if wasRunning {
			if err := current.stop(ctx, false); err != nil {
				return result, errors.Join(err, rollback())
			}
		}
		stopped = append(stopped, rollbackProject{project: current, wasRunning: wasRunning})
		preparedProject := prepared[id]
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
		preparedProject := prepared[id]
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
	if !ok {
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
	if !ok {
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
	if !ok {
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
	return id, project, nil
}

func (m *Manager) registerProjectLocked(ctx context.Context, id ID, project *Project) error {
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

func closePreparedProjects(ctx context.Context, prepared map[ID]*Project) error {
	var cleanupErr error
	for _, project := range prepared {
		cleanupErr = errors.Join(cleanupErr, project.close(ctx, false))
	}
	return cleanupErr
}

func closeStoppedProjects(ctx context.Context, stopped []rollbackProject) error {
	var cleanupErr error
	for _, item := range stopped {
		cleanupErr = errors.Join(cleanupErr, item.project.close(ctx, false))
	}
	return cleanupErr
}

func (m *Manager) waitBeforeSpawn(ctx context.Context) error {
	delay := time.Duration(0)
	if m.spawned && m.cfg.Startup.MaxSpawnPerSecond > 0 {
		delay += time.Second / time.Duration(m.cfg.Startup.MaxSpawnPerSecond)
	}
	if m.cfg.Startup.Jitter > 0 {
		delay += m.jitter(m.cfg.Startup.Jitter)
	}
	if delay <= 0 {
		return nil
	}
	return m.sleep(ctx, delay)
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
