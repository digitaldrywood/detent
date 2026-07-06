package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cli"
)

func TestKeyCommandsManageAPIKeys(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "global.yaml")
	writeGlobalConfig(t, configPath, nil)

	addOutput := runDetentKeyCommand(t, configPath,
		"key", "add",
		"--name", "Video Studio",
		"--scope", "write",
		"--project", "digitaldrywood-video",
		"--expires-in", "90d",
	)
	var created struct {
		Token string `json:"token"`
		Key   struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Key        string   `json:"key"`
			Scope      string   `json:"scope"`
			ProjectIDs []string `json:"project_ids"`
			Status     string   `json:"status"`
		} `json:"key"`
	}
	if err := json.Unmarshal(addOutput, &created); err != nil {
		t.Fatalf("Unmarshal(add) error = %v; output = %s", err, string(addOutput))
	}
	if !strings.HasPrefix(created.Token, apikey.TokenPrefix) || created.Key.ID == "" {
		t.Fatalf("created key missing token or id: %#v", created)
	}
	if created.Key.Name != "Video Studio" || created.Key.Scope != "write" || created.Key.Status != "active" {
		t.Fatalf("created key metadata = %#v", created.Key)
	}
	if len(created.Key.ProjectIDs) != 1 || created.Key.ProjectIDs[0] != "digitaldrywood-video" {
		t.Fatalf("created project ids = %#v, want digitaldrywood-video", created.Key.ProjectIDs)
	}

	listOutput := runDetentKeyCommand(t, configPath, "key", "list")
	if strings.Contains(string(listOutput), created.Token) || strings.Contains(string(listOutput), "key_hash") {
		t.Fatalf("list output leaked token or hash: %s", string(listOutput))
	}
	var listed struct {
		Keys []struct {
			ID     string `json:"id"`
			Key    string `json:"key"`
			Status string `json:"status"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(listOutput, &listed); err != nil {
		t.Fatalf("Unmarshal(list) error = %v; output = %s", err, string(listOutput))
	}
	if len(listed.Keys) != 1 || listed.Keys[0].ID != created.Key.ID || listed.Keys[0].Key == "" {
		t.Fatalf("listed keys = %#v, want created key", listed.Keys)
	}

	rotateOutput := runDetentKeyCommand(t, configPath, "key", "rotate", created.Key.ID, "--grace", "1h")
	var rotated struct {
		OldKeyID string `json:"old_key_id"`
		Token    string `json:"token"`
		Key      struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"key"`
	}
	if err := json.Unmarshal(rotateOutput, &rotated); err != nil {
		t.Fatalf("Unmarshal(rotate) error = %v; output = %s", err, string(rotateOutput))
	}
	if rotated.OldKeyID != created.Key.ID || rotated.Key.ID == "" || rotated.Token == "" || rotated.Token == created.Token {
		t.Fatalf("rotated key = %#v", rotated)
	}

	revokeOutput := runDetentKeyCommand(t, configPath, "key", "revoke", rotated.Key.ID)
	var revoked struct {
		ID      string `json:"id"`
		Revoked bool   `json:"revoked"`
	}
	if err := json.Unmarshal(revokeOutput, &revoked); err != nil {
		t.Fatalf("Unmarshal(revoke) error = %v; output = %s", err, string(revokeOutput))
	}
	if revoked.ID != rotated.Key.ID || !revoked.Revoked {
		t.Fatalf("revoked = %#v, want rotated key revoked", revoked)
	}
}

func runDetentKeyCommand(t *testing.T, configPath string, args ...string) []byte {
	t.Helper()

	cmd := cli.NewRootCommand(context.Background(), cli.WithStdoutTTY(func() bool { return false }))
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	allArgs := append([]string{"--config", configPath, "--format", "json"}, args...)
	cmd.SetArgs(allArgs)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute(%v) error = %v; stdout = %s", allArgs, err, stdout.String())
	}
	return stdout.Bytes()
}
