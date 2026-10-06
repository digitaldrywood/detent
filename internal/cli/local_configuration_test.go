package cli

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/store"
	"github.com/digitaldrywood/detent/internal/tracker"
	"gopkg.in/yaml.v3"
)

func TestLocalConfigurationStartupAndReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "global.yaml")
	workflow := filepath.Join(dir, "detent.yaml")
	if err := os.WriteFile(workflow, []byte("workflow fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := globalconfig.DefaultAt(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Projects = []globalconfig.Project{{ID: "one", Workflow: workflow, Workdir: dir, Weight: 4, Priority: 1}, {ID: "two", Workflow: workflow, Workdir: dir, Weight: 8, Priority: 2}}
	if err := globalconfig.Write(path, cfg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := yaml.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	global := document["global"].(map[string]any)
	agents, ok := global["agents"].(map[string]any)
	if !ok {
		agents = map[string]any{}
		global["agents"] = agents
	}
	agents["model_selection"] = map[string]any{"preset": "sol_first", "normal_model": "legacy-model"}
	raw, err = yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = globalconfig.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(t.Context(), store.Config{Path: filepath.Join(dir, "detent.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	first, err := initializeLocalConfiguration(t.Context(), db, cfg, logger)
	if err != nil || len(first.Projects) != 2 || first.Projects[0].ID != "two" || first.Projects[0].ModelSelection.Model("normal") != "legacy-model" {
		t.Fatalf("initial=%+v,%v", first.Projects, err)
	}
	rank, err := db.LocalProjectRank(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.LocalProjectRank(t.Context(), &projectsettings.RankChange{ExpectedRevision: rank.Revision, ProjectIDs: []tracker.ProjectID{"one", "two"}}); err != nil {
		t.Fatal(err)
	}
	model, err := db.LocalModelSelection(t.Context(), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	selection := *model.Selection
	selection.NormalModel = new("db-model")
	if _, err := db.LocalModelSelection(t.Context(), "", &projectsettings.ModelSelectionChange{ExpectedRevision: model.Revision, Selection: &selection}); err != nil {
		t.Fatal(err)
	}
	document["projects"] = 42
	agents["model_selection"] = map[string]any{"preset": "unknown"}
	raw, err = yaml.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	restart, err := readLocalConfigurationFile(path)
	if err != nil || len(restart.Projects) != 2 || restart.Projects[0].ID != "one" || restart.Projects[0].ModelSelection.Model("normal") != "db-model" {
		t.Fatalf("restart=%+v,%v", restart.Projects, err)
	}
	manager := &globalReloadManager{}
	reloader := &globalConfigReloader{current: first, manager: manager, settings: db, logger: logger}
	reloader.handle(t.Context(), configwatcher.FileUpdate[globalconfig.Config]{Path: path})
	if manager.calls != 1 || !reflect.DeepEqual(globalProjectIDs(reloader.current.Projects), []string{"one", "two"}) || manager.config.Projects[0].GlobalAgents.ModelSelection.Model("normal") != "db-model" {
		t.Fatalf("reload=%+v,calls=%d", reloader.current.Projects, manager.calls)
	}
	if err := writeLocalConfigurationFile(path, restart); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "projects:") || strings.Contains(string(raw), "model_selection:") {
		t.Fatalf("machine YAML contains project settings: %s", raw)
	}
	saved, err := readLocalConfigurationFile(path)
	if err != nil || len(saved.Projects) != 2 || saved.Global.Agents.ModelSelection.Model("normal") != "db-model" {
		t.Fatalf("saved=%+v,%v", saved.Projects, err)
	}
	candidate := globalProjectCandidates(saved.Projects)
	if candidate[0].Rank != 0 || candidate[1].Rank != 1 {
		t.Fatalf("ranks=%+v", candidate)
	}
}
