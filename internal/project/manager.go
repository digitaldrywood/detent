package project

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
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

type ProjectFactory func(globalconfig.Project) (*Project, error)

type StartupConfig struct {
	Jitter            time.Duration
	MaxSpawnPerSecond int
}

type ManagerConfig struct {
	Projects []globalconfig.Project
	Startup  StartupConfig
}

type ManagerDependencies struct {
	Registry            *Registry
	ProjectFactory      ProjectFactory
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
	factory  ProjectFactory
	sleep    func(context.Context, time.Duration) error
	jitter   func(time.Duration) time.Duration

	running bool
	spawned bool
}

func ManagerConfigFromGlobal(cfg globalconfig.Config) ManagerConfig {
	return ManagerConfig{
		Projects: append([]globalconfig.Project(nil), cfg.Projects...),
		Startup: StartupConfig{
			Jitter:            time.Duration(startupInt(cfg.Global.Startup, "jitter_seconds")) * time.Second,
			MaxSpawnPerSecond: startupInt(cfg.Global.Startup, "max_spawn_per_second"),
		},
	}
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

	cfg.Projects = append([]globalconfig.Project(nil), cfg.Projects...)
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

func (m *Manager) Remove(ctx context.Context, id ProjectID) error {
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	project, ok := m.registry.Get(id)
	if !ok {
		return ErrProjectNotFound
	}
	if project.Running() {
		if err := project.Stop(ctx); err != nil {
			return err
		}
	}
	m.registry.Delete(id)
	return nil
}

func (m *Manager) Pause(ctx context.Context, id ProjectID) error {
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

func (m *Manager) Unpause(ctx context.Context, id ProjectID) error {
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
	id := normalizeProjectID(ProjectID(cfg.ID))
	if id == "" {
		return ErrMissingProjectID
	}
	if _, ok := m.registry.Get(id); ok {
		return ErrProjectExists
	}

	cfg.ID = string(id)
	project, err := m.factory(cfg)
	if err != nil {
		return fmt.Errorf("create project %s: %w", id, err)
	}
	if err := m.registry.Set(project); err != nil {
		return err
	}
	if !m.running || project.Paused() {
		return nil
	}
	if err := m.startLocked(ctx, project); err != nil {
		m.registry.Delete(id)
		return err
	}
	return nil
}

func (m *Manager) startLocked(ctx context.Context, project *Project) error {
	if err := m.waitBeforeSpawn(ctx); err != nil {
		return err
	}
	if err := project.Start(ctx); err != nil {
		return err
	}
	m.spawned = true
	return nil
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
