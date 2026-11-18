package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/update"
)

type candidateAuthority struct {
	Identity      runnerauth.Identity
	Credential    string
	Revoked       bool
	ProjectStatus int
}

type candidateTransport func(*http.Request) (*http.Response, error)

func (f candidateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestCandidateStartupDuringIdentityTransaction(t *testing.T) {
	if os.Getenv("DETENT_TEST_STARTUP_CANDIDATE") == "1" {
		root := os.Getenv("DETENT_TEST_STARTUP_AUTHORITY")
		http.DefaultTransport = candidateTransport(func(r *http.Request) (*http.Response, error) {
			log, err := os.OpenFile(filepath.Join(root, "requests"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
			if err != nil {
				return nil, err
			}
			_, writeErr := io.WriteString(log, r.Method+" "+r.URL.Path+"\n")
			if err := errors.Join(writeErr, log.Close()); err != nil {
				return nil, err
			}
			raw, err := os.ReadFile(filepath.Join(root, "authority.json"))
			if err != nil {
				return nil, err
			}
			var authority candidateAuthority
			if err := json.Unmarshal(raw, &authority); err != nil {
				return nil, err
			}
			status := http.StatusOK
			if r.Method != http.MethodGet {
				status = http.StatusConflict
			}
			if r.Header.Get("Authorization") != "Bearer "+authority.Credential || authority.Revoked || !time.Now().Before(authority.Identity.ExpiresAt) {
				status = http.StatusUnauthorized
			}
			if strings.Contains(r.URL.Path, "/projects/prj_one") && authority.ProjectStatus != 0 {
				status = authority.ProjectStatus
				if status == http.StatusUnauthorized {
					authority.Revoked = true
					raw, err = json.Marshal(authority)
					if err != nil {
						return nil, err
					}
					if err := os.WriteFile(filepath.Join(root, "authority.json"), raw, 0600); err != nil {
						return nil, err
					}
				}
			}
			var body []byte
			if status != http.StatusOK {
				body = []byte(`{"code":"unauthorized","message":"unauthorized"}`)
			} else if strings.Contains(r.URL.Path, "/projects/") {
				body, err = json.Marshal(tracker.NativeProject{ID: tracker.ProjectID(filepath.Base(r.URL.Path)), OrganizationID: "org_example"})
			} else {
				body, err = json.Marshal(authority.Identity)
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(body))}, err
		})
		for i, arg := range os.Args {
			if arg == "--" {
				os.Args = append([]string{os.Args[0]}, os.Args[i+1:]...)
				main()
			}
		}
		os.Exit(1)
	}
	t.Setenv("DETENT_TEST_STARTUP_CANDIDATE", "1")
	for _, tt := range []struct {
		name, refusal                                                   string
		rotate, accepted, revoked, expired, invalidConfig, removedGrant bool
		projectStatus                                                   int
	}{
		{name: "renewal"},
		{name: "removed project grant", removedGrant: true},
		{name: "rotation before Hub commit", rotate: true},
		{name: "rotation after Hub commit", rotate: true, accepted: true},
		{name: "revoked authority during project read", projectStatus: http.StatusUnauthorized, refusal: "unauthorized"},
		{name: "forbidden project read", projectStatus: http.StatusForbidden, refusal: "403"},
		{name: "unavailable project remains isolated", projectStatus: http.StatusServiceUnavailable},
		{name: "missing project remains isolated", projectStatus: http.StatusNotFound},
		{name: "revoked authority", revoked: true, refusal: "unauthorized"},
		{name: "expired authority", expired: true, refusal: "unauthorized"},
		{name: "unsupported config", invalidConfig: true, refusal: "apiVersion"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := t.Context()
			root := t.TempDir()
			t.Setenv("DETENT_TEST_STARTUP_AUTHORITY", root)
			path := filepath.Join(root, "private", "identity.json")
			file, err := runnerauth.Initialize(path, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.Operations = []string{runnerauth.Read, runnerauth.Heartbeat, runnerauth.Claim, runnerauth.Events}
			file.Identity.ProjectIDs = []tracker.ProjectID{"prj_one", "prj_two"}
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			authority := candidateAuthority{Identity: file.Identity, Credential: file.Credential, Revoked: tt.revoked, ProjectStatus: tt.projectStatus}
			if tt.removedGrant {
				authority.Identity.ProjectIDs = []tracker.ProjectID{"prj_one"}
			}
			if tt.expired {
				authority.Identity.ExpiresAt = time.Now().Add(-time.Hour)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			var unblock sync.Once
			previousTransport := http.DefaultTransport
			defer func() { http.DefaultTransport = previousTransport }()
			http.DefaultTransport = candidateTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodGet {
					return &http.Response{StatusCode: http.StatusUnauthorized, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"code":"unauthorized"}`))}, nil
				}
				if tt.rotate {
					var rotation runnerauth.Rotation
					if err := json.NewDecoder(r.Body).Decode(&rotation); err != nil {
						return nil, err
					}
					if tt.accepted {
						authority.Credential = rotation.Credential
					}
				}
				raw, err := json.Marshal(authority)
				if err != nil {
					return nil, err
				}
				if err := os.WriteFile(filepath.Join(root, "authority.json"), raw, 0600); err != nil {
					return nil, err
				}
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
					return nil, r.Context().Err()
				}
				raw, err = json.Marshal(file.Identity)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, err
			})
			configPath := filepath.Join(root, "global.yaml")
			rawConfig := fmt.Sprintf("apiVersion: detent/v1\nkind: GlobalConfig\ngithub_token: fixture-token\nworkspace_root: %q\nclient:\n  hub_url: %q\n  identity_file: %q\n  organization_id: org_example\n  capacity: 1\nglobal: {}\n", filepath.Join(root, "workspaces"), file.HubURL, path)
			if err := os.WriteFile(configPath, []byte(rawConfig), 0600); err != nil {
				t.Fatal(err)
			}

			if tt.invalidConfig {
				if err := os.WriteFile(configPath, []byte("apiVersion: unsupported\nkind: DetentConfig\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			done := make(chan struct{})
			var refreshErr error
			go func() {
				_, refreshErr = hubclient.RefreshRunner(ctx, path, tt.rotate)
				close(done)
			}()
			defer func() {
				unblock.Do(func() { close(release) })
				<-done
				if refreshErr != nil {
					t.Errorf("live identity transaction failed: %v", refreshErr)
				}
			}()
			select {
			case <-entered:
			case <-done:
				t.Fatalf("live identity transaction finished before candidate startup: %v", refreshErr)
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			preflight := update.CommandPreflight("-test.run=^TestCandidateStartupDuringIdentityTransaction$", "--", "--config", configPath, "--port", "0", "--format", "json", "doctor", "--startup-preflight")
			err = preflight(ctx, os.Args[0])
			if tt.refusal == "" && err != nil || tt.refusal != "" && (err == nil || !strings.Contains(err.Error(), tt.refusal)) {
				t.Errorf("candidate preflight = %v, want refusal %q", err, tt.refusal)
			}
			after, readErr := os.ReadFile(path)
			if readErr != nil || !bytes.Equal(before, after) {
				t.Errorf("candidate changed identity: read error %v", readErr)
			}
			requests, requestErr := os.ReadFile(filepath.Join(root, "requests"))
			if strings.Contains(string(requests), "POST ") {
				t.Errorf("candidate mutated credentials: %s", requests)
			}
			if tt.refusal == "" && (requestErr != nil || !strings.Contains(string(requests), "/projects/prj_one") || strings.Contains(string(requests), "/projects/prj_two") == tt.removedGrant) {
				t.Errorf("candidate did not honor current grants: %s, %v", requests, requestErr)
			}
			lock, lockErr := instancelock.Acquire(path + ".lock")
			if lockErr == nil {
				_ = lock.Close()
			}
			if !errors.Is(lockErr, instancelock.ErrHeld) {
				t.Errorf("live identity lock = %v, want held", lockErr)
			}
		})
	}
}
