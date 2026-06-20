package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/web"
)

func TestDevRuntimeCaptureCommandPassesDefaultsToRunner(t *testing.T) {
	t.Parallel()

	outDir := filepath.Join(t.TempDir(), "capture")
	var got demoCaptureConfig
	cmd := NewRootCommand(context.Background(), func(opts *options) {
		opts.captureDemo = func(_ context.Context, cfg demoCaptureConfig) (demoCaptureResult, error) {
			got = cfg
			return demoCaptureResult{
				Version:      demoCaptureVersion,
				OutDir:       cfg.OutDir,
				ManifestPath: filepath.Join(cfg.OutDir, "demo-capture-"+demoCaptureVersion+".json"),
				Stills: []demoCaptureStillResult{{
					ScenarioID: "fleet-healthy-parallel-work",
					Path:       filepath.Join(cfg.OutDir, "stills", demoCaptureVersion, "01-fleet-healthy-parallel-work.png"),
				}},
				Terminal: demoCaptureTerminalResult{
					CastPath: filepath.Join(cfg.OutDir, "terminal", demoCaptureVersion, "onboarding.cast"),
				},
			}, nil
		}
	})
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--format", "json", "dev-runtime", "capture", "--out", outDir})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}

	if got.OutDir != outDir {
		t.Fatalf("OutDir = %q, want %q", got.OutDir, outDir)
	}
	if got.Width != 1920 || got.Height != 1080 || got.DeviceScaleFactor != 2 {
		t.Fatalf("geometry = %dx%d dsf %.1f, want 1920x1080 dsf 2", got.Width, got.Height, got.DeviceScaleFactor)
	}
	if got.DemoClock != "frozen" {
		t.Fatalf("DemoClock = %q, want frozen", got.DemoClock)
	}
	if len(got.ScenarioIDs) != 0 || got.AllScenarios {
		t.Fatalf("scenario selection = %#v all=%v, want canonical default", got.ScenarioIDs, got.AllScenarios)
	}

	var payload demoCaptureResult
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("stdout is not capture result JSON: %v\n%s", err, stdout.String())
	}
	if payload.Version != demoCaptureVersion || len(payload.Stills) != 1 || payload.Terminal.CastPath == "" {
		t.Fatalf("capture JSON = %#v, want version, still, and terminal cast", payload)
	}
}

func TestPlanDemoStillCapturesUsesCanonicalStablePaths(t *testing.T) {
	t.Parallel()

	outDir := filepath.Join(t.TempDir(), "capture")
	manifest := demoCaptureTestManifest()

	captures, err := planDemoStillCaptures(outDir, manifest, demoCaptureSelection{})
	if err != nil {
		t.Fatalf("planDemoStillCaptures() error = %v", err)
	}

	want := []string{
		"01-fleet-healthy-parallel-work.png",
		"02-fleet-kanban-multiproject.png",
		"03-kanban-full-integration.png",
		"04-project-active-overview.png",
		"05-reports-normal-window.png",
		"06-onboarding-project-selection.png",
	}
	if len(captures) != len(want) {
		t.Fatalf("captures length = %d, want %d", len(captures), len(want))
	}
	for i, name := range want {
		if captures[i].Scenario.ID != strings.TrimSuffix(strings.TrimPrefix(name[3:], "-"), ".png") {
			t.Fatalf("capture[%d] scenario = %q, want name %q", i, captures[i].Scenario.ID, name)
		}
		wantPath := filepath.Join(outDir, "stills", demoCaptureVersion, name)
		if captures[i].Path != wantPath {
			t.Fatalf("capture[%d] path = %q, want %q", i, captures[i].Path, wantPath)
		}
	}
}

func TestPlanDemoStillCapturesValidatesSelection(t *testing.T) {
	t.Parallel()

	outDir := filepath.Join(t.TempDir(), "capture")
	manifest := demoCaptureTestManifest()
	tests := []struct {
		name      string
		selection demoCaptureSelection
		wantErr   string
	}{
		{
			name:      "unknown scenario",
			selection: demoCaptureSelection{ScenarioIDs: []string{"missing"}},
			wantErr:   `unknown demo capture scenario "missing"`,
		},
		{
			name:      "post scenario",
			selection: demoCaptureSelection{ScenarioIDs: []string{"api-kanban-move-success"}},
			wantErr:   `demo capture scenario "api-kanban-move-success" uses POST`,
		},
		{
			name:      "all with explicit scenario",
			selection: demoCaptureSelection{AllScenarios: true, ScenarioIDs: []string{"fleet-healthy-parallel-work"}},
			wantErr:   "--all-scenarios cannot be combined with --scenario",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := planDemoStillCaptures(outDir, manifest, tt.selection)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("planDemoStillCaptures() error = %v, want containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestWriteDemoTerminalCaptureIsDeterministicAndIsolated(t *testing.T) {
	t.Parallel()

	outDir := filepath.Join(t.TempDir(), "capture")
	got, err := writeDemoTerminalCapture(outDir)
	if err != nil {
		t.Fatalf("writeDemoTerminalCapture() error = %v", err)
	}

	if got.CastPath != filepath.Join(outDir, "terminal", demoCaptureVersion, "onboarding.cast") {
		t.Fatalf("CastPath = %q, want stable terminal cast path", got.CastPath)
	}
	raw, err := os.ReadFile(got.CastPath)
	if err != nil {
		t.Fatalf("read cast: %v", err)
	}
	cast := string(raw)
	for _, want := range []string{
		`{"version":2,"width":100,"height":28`,
		"detent init --config ./terminal/v1/config/global.yaml",
		"detent add-project --config ./terminal/v1/config/global.yaml",
		"detent doctor --config ./terminal/v1/config/global.yaml --port 0",
		"result: PASS",
	} {
		if !strings.Contains(cast, want) {
			t.Fatalf("cast missing %q:\n%s", want, cast)
		}
	}
	for _, forbidden := range []string{"~/.config/detent/global.yaml", "/.config/detent/global.yaml", "/.detent/global.yaml"} {
		if strings.Contains(cast, forbidden) {
			t.Fatalf("cast contains non-isolated config reference %q:\n%s", forbidden, cast)
		}
	}

	head, err := os.ReadFile(filepath.Join(got.SourceRoot, ".git", "HEAD"))
	if err != nil {
		t.Fatalf("read isolated source git HEAD: %v", err)
	}
	if string(head) != "ref: refs/heads/main\n" {
		t.Fatalf("isolated source git HEAD = %q, want main ref", head)
	}
}

func demoCaptureTestManifest() []web.DemoScenarioManifest {
	ids := []string{
		"fleet-healthy-parallel-work",
		"fleet-kanban-multiproject",
		"kanban-full-integration",
		"project-active-overview",
		"reports-normal-window",
		"onboarding-project-selection",
	}
	manifest := make([]web.DemoScenarioManifest, 0, len(ids)+2)
	for _, id := range ids {
		manifest = append(manifest, web.DemoScenarioManifest{
			ID:             id,
			Route:          "/" + id,
			Method:         "GET",
			Headers:        map[string]string{web.DemoScenarioHeader: id},
			Viewport:       web.DemoViewport{Width: 1440, Height: 1100},
			ScreenshotName: id + ".png",
			WaitSelector:   "main",
		})
	}
	manifest = append(manifest,
		web.DemoScenarioManifest{
			ID:           "api-kanban-move-success",
			Route:        "/api/v1/kanban/move",
			Method:       "POST",
			WaitSelector: "body",
		},
		web.DemoScenarioManifest{
			ID:           "settings-loaded-fleet",
			Route:        "/settings",
			Method:       "GET",
			WaitSelector: "main",
		},
	)
	return manifest
}
