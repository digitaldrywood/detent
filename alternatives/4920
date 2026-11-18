//go:build !windows

package runner

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/connector"
)

func TestWorkerGitHubCLIAuthStatus(t *testing.T) {

	gh, err := exec.LookPath("gh")
	if err != nil {
		t.Fatal("GitHub CLI is required for the local CLI integration fixture")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "unix", "gh.sock")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token test-worker-token" && r.Header.Get("Authorization") != "Bearer test-worker-token" {
			t.Error("CLI did not send selected credential")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-OAuth-Scopes", "repo, read:org")
		if strings.HasSuffix(r.URL.Path, "/graphql") {
			_, _ = io.WriteString(w, `{"data":{"viewer":{"login":"worker"}}}`)
		} else {
			_, _ = io.WriteString(w, `{"login":"worker"}`)
		}
	}))
	if err := server.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	policy := workerGitHubPolicy{Token: "test-worker-token", Principal: connector.IssueActor{Login: "worker"}, GraphQLURL: "https://github.com/graphql"}
	request := AgentTurnRequest{TempDir: dir, workerGitHub: policy}
	if err := configureWorkerGitHubEnvironment(&request); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "github-cli", "config.yml"), []byte("http_unix_socket: gh.sock\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(t.Context(), gh, "auth", "status", "--hostname", policy.cliHost())
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "GH_CONFIG_DIR=" + request.Environment.Variables["GH_CONFIG_DIR"], "HOME=" + dir}
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("gh auth status: %v: %s", err, output)
	}
}
