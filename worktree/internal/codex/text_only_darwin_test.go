package codex

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestTextOnlySymlinkedRuntime(t *testing.T) {
	binary := os.Getenv("DETENT_TEST_CODEX_BINARY")
	if binary == "" {
		t.Skip("set DETENT_TEST_CODEX_BINARY to run the macOS Codex runtime regression")
	}
	binary, err := filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	home := filepath.Join(root, "home")
	for _, dir := range []string{workspace, home, filepath.Join(workspace, ".git")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	instructions := filepath.Join(workspace, "AGENTS.md")
	if err := os.WriteFile(instructions, []byte("Use example project instructions.\n"), 0600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "codex")
	if err := os.Symlink(binary, link); err != nil {
		t.Fatal(err)
	}
	env := []string{"HOME=" + home, "CODEX_HOME=" + home, "PATH=/usr/bin:/bin", "TMPDIR=" + root}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	factory, err := NewLocalTransportFactory(func(ctx context.Context) *exec.Cmd {
		cmd := exec.CommandContext(ctx, link, "app-server")
		cmd.Env = env
		return cmd
	})
	if err != nil {
		t.Fatal(err)
	}
	transport, err := factory.NewTransport(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer closeCancel()
		if err := transport.Close(closeCtx); err != nil {
			t.Error(err)
		}
	}()
	server, err := NewAppServer(factory)
	if err != nil {
		t.Fatal(err)
	}
	if err := server.initialize(ctx, transport, nil); err != nil {
		t.Fatal(err)
	}
	req := RunTurnRequest{Workspace: workspace, TextOnly: true, Permissions: "detent-text-only", RuntimeWorkspaceRoots: []string{workspace}, ApprovalPolicy: "never", Config: textOnlySettings(workspace)}
	if err := server.disableTextOnlyMCP(ctx, transport, &req); err != nil {
		t.Fatal(err)
	}
	params := map[string]any{"cwd": workspace, "approvalPolicy": "never", "config": req.Config}
	setIsolationProfile(params, req)
	if err := sendRequest(ctx, transport, threadStartRequestID, "thread/start", params); err != nil {
		t.Fatal(err)
	}
	raw, err := server.awaitResponse(ctx, transport, threadStartRequestID, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	var response struct {
		InstructionSources []string `json:"instructionSources"`
	}
	if err := json.Unmarshal(raw, &response); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, path := range response.InstructionSources {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			t.Fatal(err)
		}
		expected, err := filepath.EvalSymlinks(instructions)
		if err != nil {
			t.Fatal(err)
		}
		found = found || resolved == expected
	}
	if !found {
		t.Fatalf("workspace instructions not loaded: %s", raw)
	}
	t.Log("thread/start loaded workspace AGENTS.md through symlinked executable; no model turn requested")
	private := filepath.Join(root, "private")
	if err := os.WriteFile(private, []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer endpoint.Close()
	probe := exec.CommandContext(ctx, "/usr/bin/curl", "--fail", "--silent", "--noproxy", "*", endpoint.URL)
	probe.Env = env
	if out, err := probe.CombinedOutput(); err != nil {
		t.Fatalf("network control failed: %v %s", err, out)
	}
	for _, test := range []struct {
		name    string
		args    []string
		allowed bool
	}{
		{"workspace read", []string{"/bin/cat", instructions}, true},
		{"private read", []string{"/bin/cat", private}, false},
		{"workspace write", []string{"/bin/cp", instructions, filepath.Join(workspace, "written")}, false},
		{"network", []string{"/usr/bin/curl", "--noproxy", "*", "--connect-timeout", "1", endpoint.URL}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			filesystem := textOnlySettings(workspace)["permissions"].(map[string]any)["detent-text-only"].(map[string]any)["filesystem"].(map[string]any)
			filesystem[":minimal"] = "read"
			for _, path := range []string{"/bin/cat", "/bin/cp", "/usr/bin/curl"} {
				filesystem[path] = "read"
			}
			entries := make([]string, 0, len(filesystem))
			for path, access := range filesystem {
				entries = append(entries, string(mustJSON(t, path))+"="+string(mustJSON(t, access.(string))))
			}
			sort.Strings(entries)
			fs := "{" + strings.Join(entries, ",") + "}"
			args := []string{"-c", "permissions.detent-text-only.filesystem=" + fs, "-c", "permissions.detent-text-only.workspace_roots={" + string(mustJSON(t, workspace)) + "=true}", "-c", "permissions.detent-text-only.network.enabled=false", "sandbox", "-P", "detent-text-only", "-C", workspace}
			cmd := exec.CommandContext(ctx, binary, append(args, test.args...)...)
			cmd.Env = env
			out, err := cmd.CombinedOutput()
			if (err == nil) != test.allowed {
				t.Fatalf("sandbox error=%v output=%s", err, out)
			}

			t.Logf("allowed=%t error=%v output=%s", test.allowed, err, out)
		})
	}
}

func mustJSON(t *testing.T, value string) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
