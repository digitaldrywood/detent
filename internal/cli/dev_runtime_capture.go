package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/devruntime"
	"github.com/digitaldrywood/detent/internal/web"
)

const (
	demoCaptureVersion                  = "v1"
	demoCaptureDefaultOut               = "tmp/demo-capture"
	demoCaptureDefaultWidth             = 1920
	demoCaptureDefaultHeight            = 1080
	demoCaptureDefaultDeviceScaleFactor = 2
	demoCaptureDefaultTimeout           = 30 * time.Second
)

var demoCaptureCanonicalScenarioIDs = []string{
	"fleet-healthy-parallel-work",
	"fleet-kanban-multiproject",
	"kanban-full-integration",
	"project-active-overview",
	"reports-normal-window",
	"onboarding-project-selection",
}

type demoCaptureFunc func(context.Context, demoCaptureConfig) (demoCaptureResult, error)

type demoCaptureConfig struct {
	OutDir            string
	ScenarioIDs       []string
	AllScenarios      bool
	Width             int
	Height            int
	DeviceScaleFactor float64
	BrowserPath       string
	DemoClock         string
	Timeout           time.Duration
}

type demoCaptureResult struct {
	Version      string                    `json:"version"`
	OutDir       string                    `json:"out_dir"`
	ManifestPath string                    `json:"manifest_path"`
	Stills       []demoCaptureStillResult  `json:"stills"`
	Terminal     demoCaptureTerminalResult `json:"terminal"`
}

type demoCaptureStillResult struct {
	ScenarioID        string  `json:"scenario_id"`
	Route             string  `json:"route"`
	Path              string  `json:"path"`
	Width             int     `json:"width"`
	Height            int     `json:"height"`
	DeviceScaleFactor float64 `json:"device_scale_factor"`
}

type demoCaptureTerminalResult struct {
	CastPath     string `json:"cast_path"`
	ConfigPath   string `json:"config_path"`
	WorkflowPath string `json:"workflow_path"`
	SourceRoot   string `json:"source_root"`
}

type demoCaptureSelection struct {
	ScenarioIDs  []string
	AllScenarios bool
}

type demoStillCapture struct {
	Scenario     web.DemoScenarioManifest
	Path         string
	RelativePath string
}

type demoCaptureScenarioResponse struct {
	GeneratedAt time.Time                  `json:"generated_at"`
	Header      string                     `json:"header"`
	Clock       string                     `json:"clock"`
	Scenarios   []web.DemoScenarioManifest `json:"scenarios"`
}

type demoCaptureManifest struct {
	Version         string                    `json:"version"`
	DemoGeneratedAt time.Time                 `json:"demo_generated_at"`
	DemoClock       string                    `json:"demo_clock"`
	ScenarioHeader  string                    `json:"scenario_header"`
	Viewport        demoCaptureViewport       `json:"viewport"`
	Stills          []demoCaptureManifestItem `json:"stills"`
	Terminal        demoCaptureManifestItem   `json:"terminal"`
}

type demoCaptureViewport struct {
	Width             int     `json:"width"`
	Height            int     `json:"height"`
	DeviceScaleFactor float64 `json:"device_scale_factor"`
}

type demoCaptureManifestItem struct {
	ScenarioID   string            `json:"scenario_id,omitempty"`
	Route        string            `json:"route,omitempty"`
	Path         string            `json:"path"`
	Headers      map[string]string `json:"headers,omitempty"`
	WaitSelector string            `json:"wait_selector,omitempty"`
}

func newDevRuntimeCaptureCommand(opts options) *cobra.Command {
	cfg := demoCaptureConfig{
		OutDir:            demoCaptureDefaultOut,
		Width:             demoCaptureDefaultWidth,
		Height:            demoCaptureDefaultHeight,
		DeviceScaleFactor: demoCaptureDefaultDeviceScaleFactor,
		DemoClock:         web.DemoClockFrozen,
		Timeout:           demoCaptureDefaultTimeout,
	}
	cmd := &cobra.Command{
		Use:     "capture",
		Short:   "Capture deterministic demo screenshots and terminal casts",
		Example: "detent dev-runtime capture --out ./capture",
		Args:    NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			if err := validateDemoCaptureConfig(cfg); err != nil {
				return err
			}
			result, err := opts.captureDemo(cmd.Context(), cfg)
			if err != nil {
				return err
			}
			return out.Write(func(out io.Writer) error {
				return writeDemoCapturePretty(out, result)
			}, result)
		},
	}
	cmd.Flags().StringVar(&cfg.OutDir, "out", demoCaptureDefaultOut, "capture output directory")
	cmd.Flags().StringArrayVar(&cfg.ScenarioIDs, "scenario", nil, "scenario ID to capture; repeat for a named subset")
	cmd.Flags().BoolVar(&cfg.AllScenarios, "all-scenarios", false, "capture every browser-capturable GET scenario from the manifest")
	cmd.Flags().IntVar(&cfg.Width, "width", demoCaptureDefaultWidth, "CSS viewport width")
	cmd.Flags().IntVar(&cfg.Height, "height", demoCaptureDefaultHeight, "CSS viewport height")
	cmd.Flags().Float64Var(&cfg.DeviceScaleFactor, "device-scale-factor", demoCaptureDefaultDeviceScaleFactor, "browser device scale factor")
	cmd.Flags().StringVar(&cfg.BrowserPath, "browser", "", "Chrome or Chromium executable path")
	cmd.Flags().StringVar(&cfg.DemoClock, "demo-clock", web.DemoClockFrozen, "screenshots demo clock mode (frozen, play)")
	cmd.Flags().DurationVar(&cfg.Timeout, "timeout", demoCaptureDefaultTimeout, "capture startup and page wait timeout")
	return cmd
}

func validateDemoCaptureConfig(cfg demoCaptureConfig) error {
	switch {
	case strings.TrimSpace(cfg.OutDir) == "":
		return WrapValidation(hintedError(nil, "--out is required", exampleHint("detent dev-runtime capture --out ./capture"), "detent dev-runtime capture --out ./capture"))
	case cfg.Width <= 0:
		return WrapValidation(hintedError(nil, "--width must be positive", exampleHint("detent dev-runtime capture --width 1920"), "detent dev-runtime capture --width 1920"))
	case cfg.Height <= 0:
		return WrapValidation(hintedError(nil, "--height must be positive", exampleHint("detent dev-runtime capture --height 1080"), "detent dev-runtime capture --height 1080"))
	case cfg.DeviceScaleFactor <= 0:
		return WrapValidation(hintedError(nil, "--device-scale-factor must be positive", exampleHint("detent dev-runtime capture --device-scale-factor 2"), "detent dev-runtime capture --device-scale-factor 2"))
	case cfg.Timeout <= 0:
		return WrapValidation(hintedError(nil, "--timeout must be positive", exampleHint("detent dev-runtime capture --timeout 30s"), "detent dev-runtime capture --timeout 30s"))
	default:
		return nil
	}
}

func runDemoCapture(ctx context.Context, cfg demoCaptureConfig) (demoCaptureResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cfg.DemoClock = normalizeDemoCaptureClock(cfg.DemoClock)
	outDir, err := filepath.Abs(cfg.OutDir)
	if err != nil {
		return demoCaptureResult{}, fmt.Errorf("resolve capture output directory: %w", err)
	}
	cfg.OutDir = outDir
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return demoCaptureResult{}, fmt.Errorf("create capture output directory %s: %w", outDir, err)
	}

	terminal, err := writeDemoTerminalCapture(outDir)
	if err != nil {
		return demoCaptureResult{}, err
	}

	runtimeHome, err := os.MkdirTemp("", "detent-demo-capture-runtime-*")
	if err != nil {
		return demoCaptureResult{}, fmt.Errorf("create demo capture runtime home: %w", err)
	}
	defer func() {
		discardDemoCaptureError(os.RemoveAll(runtimeHome))
	}()

	runtime, err := devruntime.Build(devruntime.Config{
		Home:      runtimeHome,
		Demo:      devruntime.DemoScreenshots,
		DemoClock: cfg.DemoClock,
		Port:      0,
	})
	if err != nil {
		return demoCaptureResult{}, devRuntimeError(err)
	}

	runtimeCtx, cancel := context.WithCancel(ctx)
	output := &captureBuffer{}
	done := make(chan error, 1)
	go func() {
		done <- startRunning(runtimeCtx, devRuntimeBootConfig(runtime, "127.0.0.1", defaultOptions(), output))
	}()
	defer stopDemoCaptureRuntime(cancel, done)

	baseURL, err := waitForDemoCaptureRuntimeURL(ctx, output, done, cfg.Timeout)
	if err != nil {
		return demoCaptureResult{}, err
	}
	manifest, err := fetchDemoCaptureManifest(ctx, baseURL, cfg.Timeout)
	if err != nil {
		return demoCaptureResult{}, err
	}
	plans, err := planDemoStillCaptures(outDir, manifest.Scenarios, demoCaptureSelection{
		ScenarioIDs:  cfg.ScenarioIDs,
		AllScenarios: cfg.AllScenarios,
	})
	if err != nil {
		return demoCaptureResult{}, err
	}

	browser, err := startDemoCaptureBrowser(ctx, cfg)
	if err != nil {
		return demoCaptureResult{}, err
	}
	defer browser.Close()
	page, err := browser.NewPage(ctx)
	if err != nil {
		return demoCaptureResult{}, err
	}
	defer page.Close()

	stills := make([]demoCaptureStillResult, 0, len(plans))
	for _, plan := range plans {
		if err := page.CaptureScenario(ctx, baseURL, plan, cfg); err != nil {
			return demoCaptureResult{}, err
		}
		stills = append(stills, demoCaptureStillResult{
			ScenarioID:        plan.Scenario.ID,
			Route:             plan.Scenario.Route,
			Path:              plan.Path,
			Width:             cfg.Width,
			Height:            cfg.Height,
			DeviceScaleFactor: cfg.DeviceScaleFactor,
		})
	}

	manifestPath, err := writeDemoCaptureManifest(outDir, cfg, manifest, plans, terminal)
	if err != nil {
		return demoCaptureResult{}, err
	}
	return demoCaptureResult{
		Version:      demoCaptureVersion,
		OutDir:       outDir,
		ManifestPath: manifestPath,
		Stills:       stills,
		Terminal:     terminal,
	}, nil
}

func normalizeDemoCaptureClock(value string) string {
	if strings.EqualFold(strings.TrimSpace(value), web.DemoClockPlay) {
		return web.DemoClockPlay
	}
	return web.DemoClockFrozen
}

func writeDemoCapturePretty(out io.Writer, result demoCaptureResult) error {
	if _, err := fmt.Fprintf(out, "capture: %s\nmanifest: %s\n", result.OutDir, result.ManifestPath); err != nil {
		return err
	}
	if len(result.Stills) > 0 {
		if _, err := fmt.Fprintln(out, "stills:"); err != nil {
			return err
		}
		for _, still := range result.Stills {
			if _, err := fmt.Fprintf(out, "  %s %s\n", still.ScenarioID, still.Path); err != nil {
				return err
			}
		}
	}
	if strings.TrimSpace(result.Terminal.CastPath) != "" {
		if _, err := fmt.Fprintf(out, "terminal: %s\n", result.Terminal.CastPath); err != nil {
			return err
		}
	}
	return nil
}

func waitForDemoCaptureRuntimeURL(ctx context.Context, output *captureBuffer, done <-chan error, timeout time.Duration) (string, error) {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		for line := range strings.SplitSeq(output.String(), "\n") {
			if url, ok := strings.CutPrefix(line, "Dashboard: "); ok && strings.TrimSpace(url) != "" {
				return strings.TrimSpace(url), nil
			}
		}
		select {
		case err := <-done:
			return "", fmt.Errorf("demo capture runtime stopped before startup: %w\n%s", err, output.String())
		case <-waitCtx.Done():
			return "", fmt.Errorf("timed out waiting for demo capture runtime URL: %w\n%s", waitCtx.Err(), output.String())
		case <-ticker.C:
		}
	}
}

func stopDemoCaptureRuntime(cancel context.CancelFunc, done <-chan error) {
	if cancel != nil {
		cancel()
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

func fetchDemoCaptureManifest(ctx context.Context, baseURL string, timeout time.Duration) (demoCaptureScenarioResponse, error) {
	manifestURL, err := url.JoinPath(baseURL, "/api/v1/demo/scenarios")
	if err != nil {
		return demoCaptureScenarioResponse{}, fmt.Errorf("build demo scenario manifest URL: %w", err)
	}
	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return demoCaptureScenarioResponse{}, fmt.Errorf("create demo scenario manifest request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return demoCaptureScenarioResponse{}, fmt.Errorf("fetch demo scenario manifest: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return demoCaptureScenarioResponse{}, fmt.Errorf("read demo scenario manifest: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return demoCaptureScenarioResponse{}, fmt.Errorf("fetch demo scenario manifest: %s: %s", resp.Status, body)
	}
	var manifest demoCaptureScenarioResponse
	if err := json.Unmarshal(body, &manifest); err != nil {
		return demoCaptureScenarioResponse{}, fmt.Errorf("decode demo scenario manifest: %w", err)
	}
	return manifest, nil
}

func planDemoStillCaptures(outDir string, manifest []web.DemoScenarioManifest, selection demoCaptureSelection) ([]demoStillCapture, error) {
	if selection.AllScenarios && len(selection.ScenarioIDs) > 0 {
		return nil, errors.New("--all-scenarios cannot be combined with --scenario")
	}
	byID := make(map[string]web.DemoScenarioManifest, len(manifest))
	for _, scenario := range manifest {
		byID[scenario.ID] = scenario
	}

	var ids []string
	switch {
	case len(selection.ScenarioIDs) > 0:
		for _, id := range selection.ScenarioIDs {
			if id = strings.TrimSpace(id); id != "" {
				ids = append(ids, id)
			}
		}
	case selection.AllScenarios:
		for _, scenario := range manifest {
			if demoCaptureBrowserCapturableScenario(scenario) {
				ids = append(ids, scenario.ID)
			}
		}
	default:
		ids = append(ids, demoCaptureCanonicalScenarioIDs...)
	}
	if len(ids) == 0 {
		return nil, errors.New("no demo capture scenarios selected")
	}

	captures := make([]demoStillCapture, 0, len(ids))
	for i, id := range ids {
		scenario, ok := byID[id]
		if !ok {
			return nil, fmt.Errorf("unknown demo capture scenario %q", id)
		}
		if method := demoCaptureScenarioMethod(scenario); method != http.MethodGet {
			return nil, fmt.Errorf("demo capture scenario %q uses %s; screenshot capture supports GET scenarios", id, method)
		}
		if !demoCaptureBrowserCapturableScenario(scenario) {
			return nil, fmt.Errorf("demo capture scenario %q is not browser-capturable", id)
		}
		name := fmt.Sprintf("%02d-%s.png", i+1, scenario.ID)
		relativePath := filepath.ToSlash(filepath.Join("stills", demoCaptureVersion, name))
		captures = append(captures, demoStillCapture{
			Scenario:     scenario,
			Path:         filepath.Join(outDir, "stills", demoCaptureVersion, name),
			RelativePath: relativePath,
		})
	}
	return captures, nil
}

func demoCaptureBrowserCapturableScenario(scenario web.DemoScenarioManifest) bool {
	if demoCaptureScenarioMethod(scenario) != http.MethodGet {
		return false
	}
	return strings.TrimSpace(scenario.Route) != "/events"
}

func demoCaptureScenarioMethod(scenario web.DemoScenarioManifest) string {
	method := strings.ToUpper(strings.TrimSpace(scenario.Method))
	if method == "" {
		return http.MethodGet
	}
	return method
}

func writeDemoCaptureManifest(outDir string, cfg demoCaptureConfig, source demoCaptureScenarioResponse, plans []demoStillCapture, terminal demoCaptureTerminalResult) (string, error) {
	items := make([]demoCaptureManifestItem, 0, len(plans))
	for _, plan := range plans {
		items = append(items, demoCaptureManifestItem{
			ScenarioID:   plan.Scenario.ID,
			Route:        plan.Scenario.Route,
			Path:         plan.RelativePath,
			Headers:      plan.Scenario.Headers,
			WaitSelector: plan.Scenario.WaitSelector,
		})
	}
	terminalPath := filepath.ToSlash(filepath.Join("terminal", demoCaptureVersion, "onboarding.cast"))
	if relative, err := filepath.Rel(outDir, terminal.CastPath); err == nil {
		terminalPath = filepath.ToSlash(relative)
	}
	manifest := demoCaptureManifest{
		Version:         demoCaptureVersion,
		DemoGeneratedAt: source.GeneratedAt,
		DemoClock:       source.Clock,
		ScenarioHeader:  source.Header,
		Viewport: demoCaptureViewport{
			Width:             cfg.Width,
			Height:            cfg.Height,
			DeviceScaleFactor: cfg.DeviceScaleFactor,
		},
		Stills: items,
		Terminal: demoCaptureManifestItem{
			Path: terminalPath,
		},
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal demo capture manifest: %w", err)
	}
	path := filepath.Join(outDir, "demo-capture-"+demoCaptureVersion+".json")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		return "", fmt.Errorf("write demo capture manifest %s: %w", path, err)
	}
	return path, nil
}

func writeDemoTerminalCapture(outDir string) (demoCaptureTerminalResult, error) {
	root := filepath.Join(outDir, "terminal", demoCaptureVersion)
	configDir := filepath.Join(root, "config")
	sourceRoot := filepath.Join(root, "source", "detent-demo")
	workspaceRoot := filepath.Join(root, "workspaces")
	for _, dir := range []string{configDir, sourceRoot, workspaceRoot} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return demoCaptureTerminalResult{}, fmt.Errorf("create terminal capture directory %s: %w", dir, err)
		}
	}
	if err := writeDemoTerminalGitSkeleton(sourceRoot); err != nil {
		return demoCaptureTerminalResult{}, err
	}

	workflowPath := filepath.Join(configDir, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte(demoTerminalWorkflowContent()), 0o600); err != nil {
		return demoCaptureTerminalResult{}, fmt.Errorf("write terminal capture workflow: %w", err)
	}
	port := 0
	configPath := filepath.Join(configDir, "global.yaml")
	cfg := globalconfig.Config{
		Path:         configPath,
		APIVersion:   globalconfig.APIVersion,
		Kind:         globalconfig.Kind,
		Port:         &port,
		InstanceName: "detent-demo-capture",
		Global: globalconfig.Settings{
			MaxConcurrentAgents: 1,
			Scheduling:          globalconfig.SchedulingWeighted,
		},
		Projects: []globalconfig.Project{{
			ID:       "detent-demo",
			Workflow: "~/terminal/v1/config/WORKFLOW.md",
			Workdir:  "~/terminal/v1/source/detent-demo",
			Weight:   1,
			Priority: 1,
		}},
	}
	if err := globalconfig.Write(configPath, cfg, globalconfig.WithHome(outDir), globalconfig.WithRelativeTo(outDir)); err != nil {
		return demoCaptureTerminalResult{}, fmt.Errorf("write terminal capture global config: %w", err)
	}

	castPath := filepath.Join(root, "onboarding.cast")
	if err := writeDemoTerminalCast(castPath); err != nil {
		return demoCaptureTerminalResult{}, err
	}
	return demoCaptureTerminalResult{
		CastPath:     castPath,
		ConfigPath:   configPath,
		WorkflowPath: workflowPath,
		SourceRoot:   sourceRoot,
	}, nil
}

func writeDemoTerminalGitSkeleton(sourceRoot string) error {
	gitDir := filepath.Join(sourceRoot, ".git")
	for _, dir := range []string{filepath.Join(gitDir, "branches"), filepath.Join(gitDir, "objects"), filepath.Join(gitDir, "refs", "heads")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create terminal capture git directory %s: %w", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		return fmt.Errorf("write terminal capture git HEAD: %w", err)
	}
	config := "[core]\n\trepositoryformatversion = 0\n\tfilemode = true\n\tbare = false\n\tlogallrefupdates = true\n"
	if err := os.WriteFile(filepath.Join(gitDir, "config"), []byte(config), 0o644); err != nil {
		return fmt.Errorf("write terminal capture git config: %w", err)
	}
	return nil
}

func demoTerminalWorkflowContent() string {
	return strings.TrimSpace(`---
tracker:
  kind: memory
polling:
  interval_ms: 60000
workspace:
  root: ./terminal/v1/workspaces
  source_root: ./terminal/v1/source/detent-demo
  auto_branch: true
agent:
  max_concurrent_agents: 1
  max_concurrent_agents_by_state:
    Merging: 1
gate:
  kind: command
  run: "true"
server:
  host: 127.0.0.1
  port: 0
---
Deterministic Detent demo capture workflow.
`) + "\n"
}

func writeDemoTerminalCast(path string) error {
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create terminal capture cast %s: %w", path, err)
	}
	defer file.Close()

	header := struct {
		Version   int               `json:"version"`
		Width     int               `json:"width"`
		Height    int               `json:"height"`
		Timestamp int64             `json:"timestamp"`
		Env       map[string]string `json:"env"`
	}{
		Version:   2,
		Width:     100,
		Height:    28,
		Timestamp: time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC).Unix(),
		Env: map[string]string{
			"SHELL": "/bin/zsh",
			"TERM":  "xterm-256color",
		},
	}
	if err := writeAsciinemaLine(file, header); err != nil {
		return fmt.Errorf("write terminal capture cast header: %w", err)
	}
	events := []struct {
		at     float64
		output string
	}{
		{0.10, "$ detent init --config ./terminal/v1/config/global.yaml\r\n"},
		{0.45, "status: ok\r\npath: ./terminal/v1/config/global.yaml\r\nrule: --config\r\n"},
		{0.90, "$ detent add-project --config ./terminal/v1/config/global.yaml --id detent-demo --workflow ~/terminal/v1/config/WORKFLOW.md --workdir ~/terminal/v1/source/detent-demo\r\n"},
		{1.35, "status: ok\r\nid: detent-demo\r\nworkflow: ~/terminal/v1/config/WORKFLOW.md\r\nworkdir: ~/terminal/v1/source/detent-demo\r\n"},
		{1.80, "$ detent doctor --config ./terminal/v1/config/global.yaml --port 0\r\n"},
		{2.20, "Config resolution ... OK\r\nRuntime settings ... OK\r\nProject detent-demo workflow ... OK\r\nProject detent-demo source repository ... OK\r\nSQLite database ... OK\r\nGitHub token ... OK (skipped for memory tracker)\r\nServer port ... OK\r\nresult: PASS\r\n"},
	}
	for _, event := range events {
		if err := writeAsciinemaLine(file, []any{event.at, "o", event.output}); err != nil {
			return fmt.Errorf("write terminal capture cast event: %w", err)
		}
	}
	return nil
}

func writeAsciinemaLine(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func discardDemoCaptureError(error) {}

type captureBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *captureBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *captureBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
