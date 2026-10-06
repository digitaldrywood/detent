package cli

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"os"
	"slices"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"gopkg.in/yaml.v3"
)

func initializeLocalConfiguration(ctx context.Context, settings store.LocalConfigurationStore, cfg globalconfig.Config, logger *slog.Logger) (globalconfig.Config, error) {
	current, err := settings.LocalConfiguration(ctx, cfg)
	if err == nil {
		if legacyLocalSettingsPresent(cfg.Path) {
			logger.Warn("projects, weight, priority and agents.model_selection in global.yaml are ignored; local SQLite settings own projects and rank; remove legacy settings before the next release, which will reject them")
		}
		return current, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return cfg, err
	}
	var legacy struct {
		Global struct {
			Agents struct {
				ModelSelection *config.ModelSelection `yaml:"model_selection"`
			} `yaml:"agents"`
		} `yaml:"global"`
	}
	if cfg.Path != "" {
		raw, err := os.ReadFile(cfg.Path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return cfg, err
		}
		if len(raw) > 0 {
			if err := yaml.Unmarshal(raw, &legacy); err != nil {
				return cfg, err
			}
		}
	}
	if cfg.Path != "" {
		if _, err := os.Stat(cfg.Path); err == nil {
			full, err := globalconfig.Read(cfg.Path, globalconfig.WithMissingProjectPaths())
			if err != nil {
				return cfg, err
			}
			cfg.Projects = full.Projects
		} else if !errors.Is(err, os.ErrNotExist) {
			return cfg, err
		}
	}
	selection := legacy.Global.Agents.ModelSelection
	if selection == nil && cfg.Global.Agents.ModelSelection.Configured() {
		selection = &cfg.Global.Agents.ModelSelection
	}
	imported, err := settings.InitializeLocalConfiguration(ctx, cfg, selection)
	if err != nil {
		return cfg, err
	}
	if imported {
		current, err := settings.LocalConfiguration(ctx, cfg)
		if err != nil {
			return cfg, err
		}
		logger.Info("imported legacy local configuration into SQLite", "projects", globalProjectIDs(cfg.Projects), "project_rank", globalProjectIDs(current.Projects), "model_selection", selection != nil)
		if len(cfg.Projects) > 0 || selection != nil {
			logger.Warn("legacy projects, weight, priority and agents.model_selection are now owned by local SQLite settings; remove them from global.yaml before the next release, which will reject them")
		}
	}
	return settings.LocalConfiguration(ctx, cfg)
}

func readRuntimeConfiguration(ctx context.Context, settings store.LocalConfigurationStore, path string) (globalconfig.Config, error) {
	cfg, err := globalconfig.Read(path, globalconfig.WithoutProjectConfiguration(), globalconfig.WithProjectPathLiterals())
	if err != nil {
		return cfg, err
	}
	if cfg.Client.Configured() {
		return globalconfig.Read(path, globalconfig.WithProjectPathLiterals())
	}
	cfg, err = settings.LocalConfiguration(ctx, cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return globalconfig.Read(path, globalconfig.WithProjectPathLiterals())
	}
	return cfg, err
}

func writeRuntimeConfiguration(ctx context.Context, settings store.LocalConfigurationStore, cfg globalconfig.Config) error {
	if cfg.Client.Configured() {
		return globalconfig.Write(cfg.Path, cfg, globalconfig.WithProjectPathLiterals())
	}
	return settings.MutateLocalConfiguration(ctx, cfg, func(current *globalconfig.Config, _ string) bool { current.Projects = cfg.Projects; return true })
}

func readLocalConfigurationFile(path string, opts ...globalconfig.Option) (globalconfig.Config, error) {
	cfg, found, err := readLocalConfigurationProjection(path, opts...)
	if found || err != nil {
		return cfg, err
	}
	return globalconfig.Read(path, opts...)
}

func readLocalConfigurationProjectFile(path, id string, opts ...globalconfig.Option) (globalconfig.Config, []string, error) {
	cfg, found, err := readLocalConfigurationProjection(path, opts...)
	if err != nil {
		return cfg, nil, err
	}
	if !found {
		return globalconfig.ReadProject(path, id, opts...)
	}
	projects := cfg.Projects
	cfg.Projects = []globalconfig.Project{}
	skipped := []string{}
	for _, project := range projects {
		if project.ID == id {
			cfg.Projects = append(cfg.Projects, project)
		} else {
			skipped = append(skipped, project.ID)
		}
	}
	return cfg, skipped, nil
}

func readLocalConfigurationProjection(path string, opts ...globalconfig.Option) (globalconfig.Config, bool, error) {
	machineOpts := append(append([]globalconfig.Option{}, opts...), globalconfig.WithoutProjectConfiguration())
	cfg, err := globalconfig.Read(path, machineOpts...)
	if err != nil {
		return cfg, false, err
	}
	if cfg.Client.Configured() {
		return cfg, false, nil
	}
	dbPath := doctorRuntimeStorePath(cfg.Path)
	if _, err := os.Stat(dbPath); errors.Is(err, os.ErrNotExist) {
		return cfg, false, nil
	} else if err != nil {
		return cfg, false, err
	}
	settings, err := store.Open(context.Background(), store.Config{Path: dbPath})
	if err != nil {
		return cfg, false, err
	}
	defer settings.Close()
	current, err := settings.LocalConfiguration(context.Background(), cfg)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, false, nil
	}
	return current, err == nil, err
}

func writeLocalConfigurationFile(path string, cfg globalconfig.Config) error {
	if cfg.Client.Configured() {
		return globalconfig.Write(path, cfg, globalconfig.WithProjectPathLiterals())
	}
	settings, err := store.Open(context.Background(), store.Config{Path: doctorRuntimeStorePath(path)})
	if err != nil {
		return err
	}
	defer settings.Close()
	cfg.Path = path
	if err := cfg.Validate(globalconfig.WithProjectPathLiterals()); err != nil {
		return err
	}
	if _, err := initializeLocalConfiguration(context.Background(), settings, cfg, slog.Default()); err != nil {
		return err
	}
	if err := writeRuntimeConfiguration(context.Background(), settings, cfg); err != nil {
		return err
	}
	cfg.Projects = []globalconfig.Project{}
	cfg.Global.Agents.ModelSelection = config.ModelSelection{}
	return globalconfig.Write(path, cfg, globalconfig.WithProjectPathLiterals())
}

func legacyLocalSettingsPresent(path string) bool {
	raw, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	var value map[string]any
	if yaml.Unmarshal(raw, &value) != nil {
		return false
	}
	if _, present := value["projects"]; present {
		return true
	}
	global, _ := value["global"].(map[string]any)
	agents, _ := global["agents"].(map[string]any)
	_, present := agents["model_selection"]
	return present
}

func writeLocalConfigurationRank(ctx context.Context, path string, cfg globalconfig.Config, id string, position int) error {
	settings, err := store.Open(ctx, store.Config{Path: doctorRuntimeStorePath(path)})
	if err != nil {
		return err
	}
	defer settings.Close()
	cfg.Path = path
	if _, err := initializeLocalConfiguration(ctx, settings, cfg, slog.Default()); err != nil {
		return err
	}
	rank, err := settings.LocalProjectRank(ctx, nil)
	if err != nil {
		return err
	}
	projectID := tracker.ProjectID(id)
	index := slices.Index(rank.ProjectIDs, projectID)
	if index < 0 {
		return projectNotFoundError(id, cfg.Projects)
	}
	ids := slices.Delete(rank.ProjectIDs, index, index+1)
	ids = slices.Insert(ids, min(position-1, len(ids)), projectID)
	if _, err := settings.LocalProjectRank(ctx, &projectsettings.RankChange{ExpectedRevision: rank.Revision, ProjectIDs: ids}); err != nil {
		return err
	}
	cfg.Projects = []globalconfig.Project{}
	cfg.Global.Agents.ModelSelection = config.ModelSelection{}
	return globalconfig.Write(path, cfg, globalconfig.WithProjectPathLiterals())
}
