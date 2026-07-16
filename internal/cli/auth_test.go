package cli_test

import (
	"bytes"
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cli"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
)

func TestAuthLinkCommandPrintsLoginURL(t *testing.T) {
	t.Parallel()

	configPath := writeAuthCommandConfig(t)
	stdout := &bytes.Buffer{}
	command := cli.NewRootCommand(context.Background())
	command.SetOut(stdout)
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--config", configPath, "--format", "pretty", "auth", "link", "Operator@Example.com"})
	if err := command.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	link := strings.TrimSpace(stdout.String())
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("Parse(output) error = %v", err)
	}
	if parsed.Scheme != "https" || parsed.Host != "detent.example.com" || parsed.Path != "/auth/magic-link" || parsed.Query().Get("token") == "" {
		t.Fatalf("auth link output = %q", link)
	}
}

func TestAuthLinkCommandRejectsNonAllowedEmail(t *testing.T) {
	t.Parallel()

	command := cli.NewRootCommand(context.Background())
	command.SetOut(&bytes.Buffer{})
	command.SetErr(&bytes.Buffer{})
	command.SetArgs([]string{"--config", writeAuthCommandConfig(t), "--format", "pretty", "auth", "link", "other@example.com"})
	if err := command.Execute(); !errors.Is(err, auth.ErrEmailNotAllowed) {
		t.Fatalf("Execute() error = %v, want %v", err, auth.ErrEmailNotAllowed)
	}
}

func writeAuthCommandConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	workflowPath := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(workflowPath, []byte("# test workflow\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(workflow) error = %v", err)
	}
	configPath := filepath.Join(root, "global.yaml")
	cfg, err := globalconfig.DefaultAt(configPath)
	if err != nil {
		t.Fatalf("DefaultAt() error = %v", err)
	}
	cfg.Auth = globalconfig.Auth{
		Mode:          globalconfig.AuthModeMagicLink,
		PublicURL:     "https://detent.example.com",
		AllowedEmails: []string{"operator@example.com"},
		SMTP: globalconfig.SMTP{
			Host: "smtp.example.com",
			From: "detent@example.com",
		},
	}
	cfg.Projects = []globalconfig.Project{{
		ID:       "detent",
		Workflow: workflowPath,
		Workdir:  root,
		Weight:   1,
	}}
	if err := globalconfig.Write(configPath, cfg); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	return configPath
}
