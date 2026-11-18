package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestHubRunnerCommandsKeepCredentialsPrivate(t *testing.T) {
	t.Parallel()
	for _, action := range []string{"enroll", "renew", "rotate"} {
		t.Run(action, func(t *testing.T) {
			t.Parallel()
			var credential string
			var identity runnerauth.Identity
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/redeem") {
					if r.Header.Get("Authorization") != "Bearer example-enrollment-secret" {
						t.Error("incorrect enrollment authorization")
					}
					var request runnerauth.Redemption
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					credential = request.Credential
					w.WriteHeader(http.StatusCreated)
					json.NewEncoder(w).Encode(identity)
					return
				}
				if credential == "" || r.Header.Get("Authorization") != "Bearer "+credential {
					w.WriteHeader(http.StatusUnauthorized)
					json.NewEncoder(w).Encode(map[string]string{"code": "unauthorized"})
					return
				}
				if strings.HasSuffix(r.URL.Path, "/rotate") {
					var request runnerauth.Rotation
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					credential = request.Credential
				}
				json.NewEncoder(w).Encode(identity)
			}))
			t.Cleanup(server.Close)
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			file, err := runnerauth.Initialize(path, server.URL)
			if err != nil {
				t.Fatal(err)
			}
			identity = runnerauth.Identity{Binding: file.Identity.Binding, OrganizationID: "org_example", ProjectIDs: []tracker.ProjectID{"prj_example"}, Operations: []string{runnerauth.Read}, ExpiresAt: time.Now().Add(24 * time.Hour)}
			if action != "enroll" {
				file.Identity = identity
				credential = file.Credential
				if err := runnerauth.Save(path, file); err != nil {
					t.Fatal(err)
				}
			}
			command := newHubRunnerCommand("test", func(name string) string {
				if name == "DETENT_RUNNER_ENROLLMENT_TOKEN" {
					return "example-enrollment-secret"
				}
				return ""
			})
			var output bytes.Buffer
			command.SetOut(&output)
			command.SetErr(&output)
			args := []string{action, "--identity-file", path}
			if action == "enroll" {
				args = append(args, "--organization", "org_example")
			}
			command.SetArgs(args)
			if err := command.ExecuteContext(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(output.String(), identity.RunnerID) || strings.Contains(output.String(), credential) || strings.Contains(output.String(), file.Credential) || strings.Contains(output.String(), "example-enrollment-secret") {
				t.Fatal("command output omitted identity or exposed a credential")
			}
		})
	}
}

func TestHubRunnerInitRejectsWorkspaceAndRelativeFiles(t *testing.T) {
	t.Parallel()
	repository := t.TempDir()
	if err := os.WriteFile(filepath.Join(repository, ".git"), []byte("gitdir: example"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"identity.json", filepath.Join(repository, "identity.json")} {
		command := newHubRunnerCommand("test", func(string) string { return "" })
		var output bytes.Buffer
		command.SetOut(&output)
		command.SetErr(&output)
		command.SetArgs([]string{"init", "--hub-url", "https://hub.example.test", "--identity-file", path})
		if err := command.ExecuteContext(t.Context()); err == nil {
			t.Fatal("identity initialized in ordinary workspace")
		}
	}
}
