package cli

import (
	"io"
	"log/slog"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector/memory"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
)

func TestBuildChatProviderUsesConfiguredCodexBackend(t *testing.T) {
	t.Parallel()

	workflow := workflowconfig.Default()
	workflow.Tracker.Kind = workflowconfig.TrackerMemory
	workflow.Workspace.SourceRoot = t.TempDir()
	tracked, err := projectpkg.New(projectpkg.Config{
		Project:  globalconfig.Project{ID: "detent", Weight: 1},
		Workflow: workflowconfig.Workflow{Config: workflow},
	}, projectpkg.Dependencies{Connector: memory.New(memory.Config{})})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	registry := projectpkg.NewRegistry()
	if err := registry.Set(tracked); err != nil {
		t.Fatalf("Set() error = %v", err)
	}

	provider := buildChatProvider(registry, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if provider == nil {
		t.Fatal("buildChatProvider() = nil, want configured Codex provider")
	}
}

func TestBuildChatProviderWithoutProjectsIsUnavailable(t *testing.T) {
	t.Parallel()

	provider := buildChatProvider(projectpkg.NewRegistry(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if provider != nil {
		t.Fatalf("buildChatProvider() = %T, want nil", provider)
	}
}
