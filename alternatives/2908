package store

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestLocalConfiguration(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty first start", true: "legacy import"}[legacy], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "local.db")
			db, err := Open(t.Context(), Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if db != nil {
					db.Close()
				}
			})
			cfg := globalconfig.Config{Projects: []globalconfig.Project{}}
			var selection *config.ModelSelection
			want := []tracker.ProjectID{}
			if legacy {
				cfg.Projects = []globalconfig.Project{{ID: "last", Priority: 1, Weight: 9}, {ID: "first", Priority: 10, Weight: 1}, {ID: "second", Priority: 10, Weight: 8}}
				selection = &config.ModelSelection{Preset: new("sol_first"), NormalModel: new("legacy")}
				want = []tracker.ProjectID{"first", "second", "last"}
			}
			imported, err := db.InitializeLocalConfiguration(t.Context(), cfg, selection)
			if err != nil || !imported {
				t.Fatalf("import=%v,%v", imported, err)
			}
			rank, err := db.LocalProjectRank(t.Context(), nil)
			if err != nil || !reflect.DeepEqual(rank.ProjectIDs, want) {
				t.Fatalf("rank=%+v,%v", rank, err)
			}
			current, err := db.LocalConfiguration(t.Context(), cfg)
			if err != nil {
				t.Fatal(err)
			}
			for i, p := range current.Projects {
				if p.Priority != i || p.Weight != 1 || tracker.ProjectID(p.ID) != want[i] {
					t.Fatalf("project=%+v", p)
				}
				if p.ModelSelection.Model("normal") != "legacy" {
					t.Fatalf("selection=%+v", p.ModelSelection)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = Open(t.Context(), Config{Path: path})
			if err != nil {
				t.Fatal(err)
			}
			cfg.Projects = []globalconfig.Project{{ID: "should-not-import"}}
			imported, err = db.InitializeLocalConfiguration(t.Context(), cfg, &config.ModelSelection{Enabled: new(true)})
			if err != nil || imported {
				t.Fatalf("repeat import=%v,%v", imported, err)
			}
			rank, err = db.LocalProjectRank(t.Context(), nil)
			if err != nil || !reflect.DeepEqual(rank.ProjectIDs, want) {
				t.Fatalf("restart rank=%+v,%v", rank, err)
			}
		})
	}
}

func TestLocalSettingsUpdates(t *testing.T) {
	db := openTestStore(t, t.Context())
	cfg := globalconfig.Config{Projects: []globalconfig.Project{{ID: "one"}, {ID: "two"}}}
	if _, err := db.InitializeLocalConfiguration(t.Context(), cfg, nil); err != nil {
		t.Fatal(err)
	}
	rank, err := db.LocalProjectRank(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		ids      []tracker.ProjectID
		revision int64
		want     error
	}{
		{"missing", []tracker.ProjectID{"one"}, rank.Revision, projectsettings.ErrInvalid},
		{"duplicate", []tracker.ProjectID{"one", "one"}, rank.Revision, projectsettings.ErrInvalid},
		{"foreign", []tracker.ProjectID{"one", "other"}, rank.Revision, projectsettings.ErrInvalid},
		{"stale", []tracker.ProjectID{"two", "one"}, rank.Revision + 1, projectsettings.ErrConflict},
		{"reorder", []tracker.ProjectID{"two", "one"}, rank.Revision, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.LocalProjectRank(t.Context(), &projectsettings.RankChange{ExpectedRevision: tc.revision, ProjectIDs: tc.ids})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v,want=%v", err, tc.want)
			}
			saved, readErr := db.LocalProjectRank(t.Context(), nil)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if tc.want != nil && !reflect.DeepEqual(saved, rank) {
				t.Fatalf("rejected change persisted: %+v", saved)
			}
			if tc.want == nil && (got.Revision != rank.Revision+1 || !reflect.DeepEqual(got.ProjectIDs, tc.ids)) {
				t.Fatalf("saved=%+v", got)
			}
		})
	}
	org, err := db.LocalModelSelection(t.Context(), "", nil)
	if err != nil || org.Effective.Model("normal") != "gpt-6.1-sol" {
		t.Fatalf("seed=%+v,%v", org, err)
	}
	for _, tc := range []struct {
		name      string
		project   tracker.ProjectID
		selection *config.ModelSelection
		revision  tracker.Revision
		want      error
		model     string
	}{
		{"invalid org", "", &config.ModelSelection{Enabled: new(true)}, org.Revision, projectsettings.ErrInvalid, ""},
		{"org default", "", func() *config.ModelSelection { p := *org.Selection; p.NormalModel = new("org-model"); return &p }(), org.Revision, nil, "org-model"},
		{"project partial", "one", &config.ModelSelection{NormalModel: new("project-model")}, 1, nil, "project-model"},
		{"stale project", "one", nil, 1, projectsettings.ErrConflict, ""},
		{"project inherits", "one", nil, 2, nil, "org-model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := db.LocalModelSelection(t.Context(), tc.project, &projectsettings.ModelSelectionChange{ExpectedRevision: tc.revision, Selection: tc.selection})
			if !errors.Is(err, tc.want) {
				t.Fatalf("error=%v,want=%v", err, tc.want)
			}
			if err == nil && (got.Effective.Model("normal") != tc.model || *got.Effective.Stages["plan"].Effort != "low") {
				t.Fatalf("model=%+v", got)
			}
		})
	}
	allowed, err := db.LocalAllowedProjects(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]tracker.ProjectID{{"other"}, {"one", "one"}} {
		if _, err := db.LocalAllowedProjects(t.Context(), &AllowedProjectsChange{ExpectedRevision: allowed.Revision, ProjectIDs: ids}); !errors.Is(err, projectsettings.ErrInvalid) {
			t.Fatalf("invalid grants=%v", err)
		}
	}
	if _, err := db.LocalAllowedProjects(t.Context(), &AllowedProjectsChange{ExpectedRevision: allowed.Revision, ProjectIDs: []tracker.ProjectID{"one"}}); err != nil {
		t.Fatal(err)
	}
	current, err := db.LocalConfiguration(t.Context(), cfg)
	if err != nil || len(current.Projects) != 1 || current.Projects[0].ID != "one" || current.Projects[0].Priority != 1 || current.Projects[0].ModelSelection.Model("normal") != "org-model" {
		t.Fatalf("runtime=%+v,%v", current.Projects, err)
	}
	rank, err = db.LocalProjectRank(t.Context(), nil)
	if err != nil || len(rank.ProjectIDs) != 2 {
		t.Fatalf("grant removal deleted project: %+v,%v", rank, err)
	}
	err = db.MutateLocalConfiguration(t.Context(), cfg, func(current *globalconfig.Config, _ string) bool {
		selection, err := db.LocalModelSelection(t.Context(), "", nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.LocalModelSelection(t.Context(), "", &projectsettings.ModelSelectionChange{ExpectedRevision: selection.Revision, Selection: &config.ModelSelection{Preset: new("sol_first"), NormalModel: new("newer")}}); err != nil {
			t.Fatal(err)
		}
		current.Projects[0].Paused = true
		return true
	})
	if !errors.Is(err, projectsettings.ErrConflict) {
		t.Fatalf("concurrent mutation=%v", err)
	}
}

func TestLocalImportAtomic(t *testing.T) {
	db := openTestStore(t, t.Context())
	cfg := globalconfig.Config{Projects: []globalconfig.Project{{ID: "one"}}}
	if _, err := db.InitializeLocalConfiguration(t.Context(), cfg, &config.ModelSelection{Enabled: new(true)}); !errors.Is(err, projectsettings.ErrInvalid) {
		t.Fatalf("invalid import=%v", err)
	}
	if imported, err := db.InitializeLocalConfiguration(t.Context(), cfg, nil); err != nil || !imported {
		t.Fatalf("retry=%v,%v", imported, err)
	}
}
