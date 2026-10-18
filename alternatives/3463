package workitem_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	"github.com/digitaldrywood/detent/internal/connector/local"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/workitem"
)

func TestNonCodeArtifactDemoFixtures(t *testing.T) {
	t.Parallel()

	root := filepath.Join("..", "..")
	demoDir := filepath.Join(root, "docs", "examples", "non-code-artifact")
	workflowPath := filepath.Join(demoDir, "WORKFLOW.md")

	workflow, err := workflowconfig.LoadWorkflow(workflowPath)
	if err != nil {
		t.Fatalf("LoadWorkflow() error = %v", err)
	}
	if err := workflow.Config.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	cfg := workflow.Config
	if cfg.Tracker.Kind != workflowconfig.TrackerLocalSQLite {
		t.Fatalf("Tracker.Kind = %q, want local_sqlite", cfg.Tracker.Kind)
	}
	if cfg.Workspace.Kind != workflowconfig.WorkspaceFilesystem {
		t.Fatalf("Workspace.Kind = %q, want filesystem", cfg.Workspace.Kind)
	}
	if cfg.Deliverable.Kind != workflowconfig.DeliverableArtifact {
		t.Fatalf("Deliverable.Kind = %q, want artifact", cfg.Deliverable.Kind)
	}
	if cfg.Gate.Kind != gate.KindArtifact {
		t.Fatalf("Gate.Kind = %q, want artifact", cfg.Gate.Kind)
	}
	if cfg.Gate.Artifact.StatusField != "render_status" {
		t.Fatalf("Gate.Artifact.StatusField = %q, want render_status", cfg.Gate.Artifact.StatusField)
	}

	for _, state := range []string{"Research", "Draft", "Review", "Rework", "Package", "Publish"} {
		if !stateConfigured(cfg, state) {
			t.Fatalf("workflow state %q is not configured", state)
		}
	}
	for _, status := range []string{"queued", "pending_review"} {
		if !slices.Contains(cfg.Gate.Artifact.WaitStatuses, status) {
			t.Fatalf("wait statuses = %#v, want %q", cfg.Gate.Artifact.WaitStatuses, status)
		}
	}
	if !slices.Contains(cfg.Gate.Artifact.PassStatuses, "approved") {
		t.Fatalf("pass statuses = %#v, want approved", cfg.Gate.Artifact.PassStatuses)
	}
	if !slices.Contains(cfg.Gate.Artifact.ReworkStatuses, "recut") {
		t.Fatalf("rework statuses = %#v, want recut", cfg.Gate.Artifact.ReworkStatuses)
	}

	seenStates := map[string]bool{}
	seenStatuses := map[string]bool{}
	for _, issue := range cfg.Tracker.Issues {
		if issue.AssignedToWorker {
			t.Fatalf("seed issue %s is assigned to worker; demo seeds must not dispatch agents", issue.ID)
		}
		if issue.Fields["render_status"] == "" {
			t.Fatalf("seed issue %s missing render_status field", issue.ID)
		}
		if issue.Deliverable == nil {
			t.Fatalf("seed issue %s missing artifact deliverable", issue.ID)
		}
		if strings.TrimSpace(issue.Deliverable.ValidationStatus) != "" {
			t.Fatalf("seed issue %s deliverable validation status overrides render_status", issue.ID)
		}
		if issue.Deliverable.Metadata["artifact_gate"] != "render_status" {
			t.Fatalf("seed issue %s deliverable metadata = %#v, want artifact_gate render_status", issue.ID, issue.Deliverable.Metadata)
		}
		seenStates[issue.State] = true
		seenStatuses[issue.Fields["render_status"]] = true
	}
	for _, state := range []string{"Research", "Draft", "Review", "Package"} {
		if !seenStates[state] {
			t.Fatalf("seed issues missing state %q; states = %#v", state, seenStates)
		}
	}
	for _, status := range []string{"queued", "pending_review", "approved", "recut"} {
		if !seenStatuses[status] {
			t.Fatalf("seed issues missing render_status %q; statuses = %#v", status, seenStatuses)
		}
	}

	validateSeedPayloads(t, demoDir, cfg)
	validateManifest(t, filepath.Join(demoDir, "output", "package-approved-newsletter", "manifest.json"))
}

func validateSeedPayloads(t *testing.T, demoDir string, cfg workflowconfig.Config) {
	t.Helper()

	conn, err := local.New(local.Config{
		Path:           filepath.Join(t.TempDir(), "work-items.db"),
		ProjectID:      cfg.Tracker.LocalSQLite.ProjectID,
		Issues:         cfg.Tracker.Issues,
		ActiveStates:   cfg.Tracker.ActiveStates,
		ObservedStates: cfg.Tracker.ObservedStates,
		TerminalStates: cfg.Tracker.TerminalStates,
	})
	if err != nil {
		t.Fatalf("local.New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Fatalf("Close() error = %v", err)
		}
	})

	entries, err := os.ReadDir(filepath.Join(demoDir, "seed"))
	if err != nil {
		t.Fatalf("ReadDir(seed) error = %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("seed payloads = 0, want at least one API example")
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(demoDir, "seed", entry.Name()))
		if err != nil {
			t.Fatalf("ReadFile(%s) error = %v", entry.Name(), err)
		}
		var req workitem.Request
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Fatalf("Unmarshal(%s) error = %v", entry.Name(), err)
		}
		if req.Fields["render_status"] == "" {
			t.Fatalf("%s missing render_status field", entry.Name())
		}
		if req.Deliverable == nil || req.Deliverable.Kind != workflowconfig.DeliverableArtifact {
			t.Fatalf("%s deliverable = %#v, want artifact", entry.Name(), req.Deliverable)
		}
		if strings.TrimSpace(req.Deliverable.ValidationStatus) != "" {
			t.Fatalf("%s deliverable validation status overrides render_status", entry.Name())
		}
		if _, err := workitem.Create(context.Background(), workitem.Target{
			ProjectID: "default",
			Workflow:  cfg,
			Connector: conn,
		}, req); err != nil {
			t.Fatalf("Create(%s) error = %v", entry.Name(), err)
		}
	}
}

func validateManifest(t *testing.T, path string) {
	t.Helper()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(manifest) error = %v", err)
	}
	var manifest struct {
		WorkItem struct {
			ID         string `json:"id"`
			Identifier string `json:"identifier"`
			Title      string `json:"title"`
		} `json:"work_item"`
		SourceAssets []string `json:"source_assets"`
		Artifact     struct {
			Kind      string `json:"kind"`
			Path      string `json:"path"`
			ReviewURL string `json:"review_url"`
		} `json:"artifact"`
		Validation struct {
			StatusField string `json:"status_field"`
			Status      string `json:"status"`
			Notes       string `json:"notes"`
		} `json:"validation"`
		NextExternalAction string `json:"next_external_action"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("Unmarshal(manifest) error = %v", err)
	}
	if manifest.WorkItem.ID != "content-demo-004" || manifest.WorkItem.Identifier == "" || manifest.WorkItem.Title == "" {
		t.Fatalf("manifest work item = %#v", manifest.WorkItem)
	}
	if len(manifest.SourceAssets) == 0 {
		t.Fatal("manifest source_assets is empty")
	}
	if manifest.Artifact.Kind == "" || manifest.Artifact.Path == "" || manifest.Artifact.ReviewURL == "" {
		t.Fatalf("manifest artifact = %#v", manifest.Artifact)
	}
	if manifest.Validation.StatusField != "render_status" || manifest.Validation.Status != "approved" || manifest.Validation.Notes == "" {
		t.Fatalf("manifest validation = %#v", manifest.Validation)
	}
	if manifest.NextExternalAction == "" {
		t.Fatal("manifest next_external_action is empty")
	}
}

func stateConfigured(cfg workflowconfig.Config, state string) bool {
	for _, candidate := range append(append([]string{}, cfg.Tracker.ActiveStates...), append(cfg.Tracker.ObservedStates, cfg.Tracker.TerminalStates...)...) {
		if strings.EqualFold(strings.TrimSpace(candidate), state) {
			return true
		}
	}
	return false
}
