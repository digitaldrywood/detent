package cli_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
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

func TestAuthTokenEnableAndRotate(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, path, nil)

	enabledURL := runDashboardAuthCommand(t, path, "enable", "--allow-write", "--base-url", "https://detent.example.com")
	enabledToken := dashboardTokenFromURL(t, enabledURL)
	cfg, err := globalconfig.Read(path)
	if err != nil {
		t.Fatalf("Read() after enable error = %v", err)
	}
	if cfg.DashboardAccess.Mode != globalconfig.DashboardAccessModePrivateToken || !cfg.DashboardAccess.AllowWrite {
		t.Fatalf("DashboardAccess after enable = %#v", cfg.DashboardAccess)
	}
	if cfg.DashboardAccess.Token != enabledToken {
		t.Fatalf("stored token differs from enable URL")
	}

	rotatedURL := runDashboardAuthCommand(t, path, "rotate", "--base-url", "https://detent.example.com")
	rotatedToken := dashboardTokenFromURL(t, rotatedURL)
	if rotatedToken == enabledToken {
		t.Fatal("rotated token equals enabled token")
	}
	cfg, err = globalconfig.Read(path)
	if err != nil {
		t.Fatalf("Read() after rotate error = %v", err)
	}
	if cfg.DashboardAccess.Token != rotatedToken || !cfg.DashboardAccess.AllowWrite {
		t.Fatalf("DashboardAccess after rotate = %#v", cfg.DashboardAccess)
	}
}

func TestAuthTokenRotateRequiresEnabledMode(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, path, nil)
	cmd := cli.NewRootCommand(context.Background())
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"--config", path, "auth", "token", "rotate"})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("Execute() error = %v, want not enabled", err)
	}
}

func runDashboardAuthCommand(t *testing.T, path string, operation string, args ...string) string {
	t.Helper()

	commandArgs := []string{"--config", path, "--format", "json", "auth", "token", operation}
	commandArgs = append(commandArgs, args...)
	cmd := cli.NewRootCommand(context.Background())
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs(commandArgs)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(%s) error = %v", operation, err)
	}
	var result struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("Unmarshal(%s) error = %v; output = %s", operation, err, stdout.String())
	}
	return result.URL
}

func dashboardTokenFromURL(t *testing.T, value string) string {
	t.Helper()

	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatalf("Parse(%q) error = %v", value, err)
	}
	if parsed.Scheme != "https" || parsed.Host != "detent.example.com" || parsed.Path != "/" {
		t.Fatalf("dashboard URL = %q, want https://detent.example.com/", value)
	}
	token := parsed.Query().Get("token")
	decoded, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil || len(decoded) != 32 {
		t.Fatalf("dashboard token is not 256-bit URL-safe base64: %q", token)
	}
	return token
}
