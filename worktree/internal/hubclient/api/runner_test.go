package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/skills"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRunnerRequestsDoNotFollowCredentialRedirects(t *testing.T) {
	if testing.Short() {
		t.Skip("loopback network listener integration")
	}

	t.Parallel()
	var forwarded atomic.Int64
	recipient := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded.Add(1); w.WriteHeader(http.StatusOK) }))
	t.Cleanup(recipient.Close)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, recipient.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(server.Close)
	client, err := New(Config{URL: server.URL, TokenSource: func() string { return "example-enrollment-token" }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.CreateRunnerEnrollment(t.Context(), "org_example", runnerauth.EnrollmentRequest{})
	if err == nil || forwarded.Load() != 0 {
		t.Fatalf("credential redirect followed: requests=%d err=%v", forwarded.Load(), err)
	}
}

func TestRunnerAvailabilityHeartbeatAndClaim(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "private", "runner.json")
	file, err := runnerauth.Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	file.Identity.OrganizationID = "org_test"
	file.Identity.ExpiresAt = time.Now().Add(24 * time.Hour)
	if err := runnerauth.Save(path, file); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	closed := runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun " + now.Add(time.Hour).Format("15:04") + "-" + now.Add(2*time.Hour).Format("15:04")}}
	snapshot := runnerauth.RoutingSnapshot{RunnerID: file.Identity.RunnerID, Revision: 1, Routing: runnerauth.Routing{DisplayName: "Runner", State: "active", CapacityLimit: 3, Availability: closed}.Normalized()}
	inventory := []skills.ProviderSkill{{Name: "plan", Scope: "project", Path: ".agents/skills/plan/SKILL.md", Invocation: "$plan", Enabled: true, UserInvocable: true}}
	var capacities []int
	claims := 0
	transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
		body := `{}`
		if strings.HasSuffix(request.URL.Path, "/capabilities") {
			body = `{"features":["runner_project_skills"]}`
		} else if strings.HasSuffix(request.URL.Path, "/heartbeat") {
			var report struct {
				Capacity int                    `json:"capacity"`
				Skills   []skills.ProviderSkill `json:"skills"`
			}
			if err := json.NewDecoder(request.Body).Decode(&report); err != nil {
				return nil, err
			}
			if !reflect.DeepEqual(report.Skills, inventory) {
				t.Errorf("heartbeat skill inventory = %#v, want %#v", report.Skills, inventory)
			}
			capacities = append(capacities, report.Capacity)
			encoded, err := json.Marshal(snapshot)
			if err != nil {
				return nil, err
			}
			body = string(encoded)
		} else if strings.HasSuffix(request.URL.Path, "/claims") {
			claims++
			body = `{"isolation_policy":{"tier":"sandbox"}}`
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}, nil
	})
	client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	machine := Machine{ID: file.Identity.MachineID, Capacity: 3, Version: "test", Skills: &inventory}
	if err := native.RegisterMachine(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	if len(capacities) == 0 || capacities[0] != 0 || capacities[len(capacities)-1] != 0 {
		t.Fatalf("closed capacities = %v", capacities)
	}
	if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); !errors.Is(err, ErrNoClaimableWork) || claims != 0 {
		t.Fatalf("closed claim = %v, calls = %d", err, claims)
	}
	restarted, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	native, err = restarted.Native("org_test", "prj_test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); !errors.Is(err, ErrNoClaimableWork) || claims != 0 {
		t.Fatalf("cached claim = %v, calls = %d", err, claims)
	}
	snapshot.Revision++
	snapshot.Routing.Availability = runnerauth.Availability{}
	if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
		t.Fatal(err)
	}
	if capacities[len(capacities)-1] != 3 {
		t.Fatalf("reopened capacities = %v", capacities)
	}
	for _, test := range []struct {
		name  string
		slots map[string]int
		want  int
	}{
		{"counted", map[string]int{"Mon-Sun 00:00-24:00": 2}, 2},
		{"lower count", map[string]int{"Mon-Sun 00:00-24:00": 1}, 1},
		{"above reported", map[string]int{"Mon-Sun 00:00-24:00": 6}, 3},
		{"zero count", map[string]int{"Mon-Sun 00:00-24:00": 0}, 0},
		{"uncounted", nil, 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			snapshot.Revision++
			snapshot.Routing.Availability = runnerauth.Availability{Timezone: "UTC", Windows: []string{"Mon-Sun 00:00-24:00"}, WindowSlots: test.slots}
			if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
				t.Fatal(err)
			}
			if capacities[len(capacities)-1] != test.want {
				t.Fatalf("capacities = %v, want latest %d", capacities, test.want)
			}
			capacities = nil
			if err := native.HeartbeatMachine(t.Context(), machine); err != nil {
				t.Fatal(err)
			}
			if len(capacities) != 1 || capacities[0] != test.want {
				t.Fatalf("cached capacities = %v, want [%d]", capacities, test.want)
			}
			if test.want == 0 {
				if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); !errors.Is(err, ErrNoClaimableWork) || claims != 0 {
					t.Fatalf("zero-count claim = %v, calls = %d", err, claims)
				}
			}
		})
	}
	if _, err := native.Claim(t.Context(), tracker.NativeClaim{}); err != nil || claims != 1 {
		t.Fatalf("open claim = %v, calls = %d", err, claims)
	}
}

func TestUnchangedCredentialReadLeavesLockMetadataUnwritten(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		clients  int
		lockFile bool
	}{
		{name: "repeated requests", clients: 1, lockFile: true},
		{name: "concurrent clients", clients: 8, lockFile: true},
		{name: "missing lock file", clients: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			file, err := runnerauth.Initialize(path, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.Operations = []string{runnerauth.Read}
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			lockPath := path + ".lock"
			var lockInfo os.FileInfo
			if test.lockFile {
				if err := os.WriteFile(lockPath, nil, 0o600); err != nil {
					t.Fatal(err)
				}
				old := time.Unix(946684800, 0)
				if err := os.Chtimes(lockPath, old, old); err != nil {
					t.Fatal(err)
				}
				lockInfo, err = os.Stat(lockPath)
				if err != nil {
					t.Fatal(err)
				}
			}
			var requests atomic.Int32
			transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet || r.URL.Path != "/api/v2/organizations/org_example/projects/prj_example" || r.Header.Get("Authorization") != "Bearer "+file.Credential {
					t.Errorf("unexpected authenticated project request: %s %s", r.Method, r.URL.Path)
				}
				requests.Add(1)
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"project_id":"prj_example","organization_id":"org_example"}`))}, nil
			})
			var wg sync.WaitGroup
			for range test.clients {
				client, err := New(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				native, err := client.Native("org_example", "prj_example")
				if err != nil {
					t.Fatal(err)
				}
				wg.Go(func() {
					for range 3 {
						project, err := native.Project(t.Context())
						if err != nil || project.ID != "prj_example" || project.OrganizationID != "org_example" {
							t.Errorf("authenticated project read: %+v, %v", project, err)
						}
					}
				})
			}
			wg.Wait()
			if got := requests.Load(); got != int32(test.clients*3) {
				t.Fatalf("authenticated requests = %d, want %d", got, test.clients*3)
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(before, after) {
				t.Fatalf("unchanged credential read modified the identity: %v", err)
			}
			afterLock, err := os.Stat(lockPath)
			if !test.lockFile {
				if !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unchanged credential read created a persistent lock file: %v", err)
				}
			} else if err != nil || !os.SameFile(lockInfo, afterLock) || !afterLock.ModTime().Equal(lockInfo.ModTime()) {
				t.Fatalf("a GET using an unchanged valid credential rewrote persistent lock metadata: %v", err)
			}
		})
	}
}

func TestLockIdentityFileSerializesCallersInOneProcess(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.json")
	for _, test := range []struct {
		name    string
		callers int
	}{
		{name: "two callers", callers: 2},
		{name: "many callers", callers: 32},
	} {
		t.Run(test.name, func(t *testing.T) {
			var holders, maxHolders atomic.Int32
			var wg sync.WaitGroup
			errs := make(chan error, test.callers)
			for range test.callers {
				wg.Go(func() {
					release, err := lockIdentityFile(path)
					if err != nil {
						errs <- err
						return
					}
					current := holders.Add(1)
					for {
						seen := maxHolders.Load()
						if current <= seen || maxHolders.CompareAndSwap(seen, current) {
							break
						}
					}
					time.Sleep(time.Millisecond)
					holders.Add(-1)
					if err := release(); err != nil {
						errs <- err
					}
				})
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Fatalf("lockIdentityFile() error = %v", err)
			}
			if maxHolders.Load() != 1 {
				t.Fatalf("concurrent holders = %d, want 1", maxHolders.Load())
			}
		})
	}
}

func TestLockIdentityFileRefusesHeldFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "identity.json")
	file, err := runnerauth.Initialize(path, "https://hub.example.test")
	if err != nil {
		t.Fatal(err)
	}
	client, err := New(Config{URL: file.HubURL, IdentityFile: path})
	if err != nil {
		t.Fatal(err)
	}
	held, err := instancelock.Acquire(path + ".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if _, err := lockIdentityFile(path); !errors.Is(err, instancelock.ErrHeld) {
		t.Fatalf("lockIdentityFile() error = %v, want ErrHeld", err)
	}
	for _, test := range []struct {
		name    string
		expired bool
		pending bool
		refresh bool
	}{
		{name: "unchanged read"},
		{name: "expired credential", expired: true},
		{name: "pending rotation", pending: true},
		{name: "identity refresh", refresh: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if test.expired {
				file.Identity.ExpiresAt = time.Now().Add(-time.Minute)
			}
			file.PendingCredential = ""
			if test.pending {
				file.PendingCredential = file.Credential
			}
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			got, err := client.runnerCredential(t.Context(), test.refresh)
			if test.expired || test.pending || test.refresh {
				if !errors.Is(err, instancelock.ErrHeld) {
					t.Fatalf("credential mutation bypassed the writer lock: %v", err)
				}
			} else if err != nil || got.Credential != file.Credential {
				t.Fatalf("unchanged credential read required a writer lock: %v", err)
			}
		})
	}
	release, err := lockIdentityFile(filepath.Join(t.TempDir(), "other.json"))
	if err != nil {
		t.Fatalf("lockIdentityFile(other) error = %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerReaderReconcilesAtomicRotation(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name      string
		pending   bool
		protected bool
		expired   bool
		wrongHost bool
		wantErr   string
	}{
		{name: "pending credential accepted", pending: true},
		{name: "rotation persisted"},
		{name: "pending rotation during protected read", pending: true, protected: true},
		{name: "persisted rotation during protected read", protected: true},
		{name: "expired rotation during protected read", protected: true, expired: true, wantErr: "expired"},
		{name: "Hub binding changed during protected read", protected: true, wrongHost: true, wantErr: "binding cannot be replaced"},
		{name: "expired Hub response", expired: true, wantErr: "expired"},
		{name: "Hub binding changed", wrongHost: true, wantErr: "binding cannot be replaced"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "private", "identity.json")
			file, err := runnerauth.Initialize(path, "https://hub.example.test")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.Operations = []string{runnerauth.Read}
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			replacement, err := runnerauth.Initialize(filepath.Join(t.TempDir(), "private", "replacement.json"), file.HubURL)
			if err != nil {
				t.Fatal(err)
			}
			calls, protectedCalls := 0, 0
			var rotated []byte
			transport := executionRoundTrip(func(r *http.Request) (*http.Response, error) {
				calls++
				if r.Method != http.MethodGet {
					t.Fatalf("reader attempted %s", r.Method)
				}
				status := http.StatusOK
				body := `{"code":"unauthorized"}`
				protected := strings.Contains(r.URL.Path, "/projects/")
				if protected {
					protectedCalls++
				}
				if tt.protected && calls == 1 {
					raw, err := json.Marshal(file.Identity)
					return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(raw))}, err
				}
				if !tt.protected && calls == 1 || tt.protected && protectedCalls == 1 && protected {
					release, err := lockIdentityFile(path)
					if err != nil {
						return nil, err
					}
					file.PendingCredential = replacement.Credential
					if !tt.pending {
						file.Credential, file.PendingCredential = replacement.Credential, ""
					}
					if err := errors.Join(runnerauth.Save(path, file), release()); err != nil {
						return nil, err
					}
					rotated, err = os.ReadFile(path)
					if err != nil {
						return nil, err
					}
					if tt.wrongHost {
						file.HubURL = "https://other.example.test"
						rotated, err = json.Marshal(file)
						if err != nil {
							return nil, err
						}
						if err := os.WriteFile(path, rotated, 0600); err != nil {
							return nil, err
						}
					}

					status = http.StatusUnauthorized
				} else {
					if r.Header.Get("Authorization") != "Bearer "+replacement.Credential {
						t.Fatal("reader did not use the rotated credential")
					}
					identity := file.Identity
					if tt.expired {
						identity.ExpiresAt = time.Now().Add(-time.Hour)
					}
					raw, err := json.Marshal(identity)
					if protected {
						raw, err = json.Marshal(tracker.NativeProject{ID: "prj_example", OrganizationID: "org_example"})
					}
					if err != nil {
						return nil, err
					}
					body = string(raw)
				}
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			client, err := NewRunnerReader(Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{Transport: transport}})
			if err != nil {
				t.Fatal(err)
			}
			if tt.protected {
				native, nativeErr := client.Native("org_example", "prj_example")
				if nativeErr != nil {
					t.Fatal(nativeErr)
				}
				remote, readErr := native.Project(t.Context())
				err = readErr
				if tt.wantErr == "" && (err != nil || remote.ID != "prj_example" || remote.OrganizationID != "org_example") {
					t.Fatalf("protected project = %+v, %v", remote, err)
				}
			} else {
				var identity runnerauth.Identity
				identity, err = client.RunnerIdentity(t.Context())
				if tt.wantErr == "" && identity.Binding != file.Identity.Binding {
					t.Fatalf("reader identity = %+v, want binding %+v", identity, file.Identity.Binding)
				}
			}
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("reader error = %v; want %q", err, tt.wantErr)
			}
			if !tt.protected && calls > 2 || calls > 4 || protectedCalls > 2 {
				t.Fatalf("reader requests = %d, want at most two credential reads", calls)
			}
			after, err := os.ReadFile(path)
			if err != nil || string(after) != string(rotated) {
				t.Fatalf("reader overwrote rotation: %v", err)
			}
		})
	}
}
