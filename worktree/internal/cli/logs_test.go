package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	ghconnector "github.com/digitaldrywood/detent/internal/connector/github"
	"github.com/digitaldrywood/detent/internal/instancelock"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestEnrolledReadCommandsDuringIdentityWrite(t *testing.T) {
	for _, name := range []string{"runner_alpha", "runner_beta"} {
		t.Run(name, func(t *testing.T) {
			projectID := tracker.ProjectID("prj_" + name)
			hub := newRegisterHub(t, map[tracker.ProjectID]string{projectID: "Example project"})
			hub.repositories[projectID] = "owner/repository"
			var projectStatus, projectReads atomic.Int32
			readerHub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("diagnostic attempted a Hub mutation: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				if strings.HasSuffix(r.URL.Path, "/projects/"+string(projectID)) {
					projectReads.Add(1)
					if status := projectStatus.Load(); status != 0 {
						w.WriteHeader(int(status))
						return
					}
				}
				hub.server.Config.Handler.ServeHTTP(w, r)
			}))
			t.Cleanup(readerHub.Close)
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			identityPath := filepath.Join(dir, "identity.json")
			file, err := runnerauth.Initialize(identityPath, readerHub.URL)
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = "org_example"
			file.Identity.ProjectIDs = []tracker.ProjectID{projectID}
			file.Identity.Operations = []string{runnerauth.Read}
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if err := runnerauth.Save(identityPath, file); err != nil {
				t.Fatal(err)
			}
			hub.identity.Store(file.Identity)
			hub.credential.Store(file.Credential)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture_service_token" {
					t.Error("state request lost the configured service token")
				}
				if r.Method != http.MethodGet || r.URL.Path != "/api/v1/state" {
					t.Errorf("unexpected dashboard request: %s %s", r.Method, r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(stateFixture())
			}))
			t.Cleanup(server.Close)
			address, err := url.Parse(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(dir, "global.yaml")
			config := fmt.Sprintf("apiVersion: detent/v1\nkind: GlobalConfig\nport: %s\napi_token: fixture_service_token\nworkspace_root: %s\nclient:\n  hub_url: %s\n  organization_id: org_example\n  identity_file: %s\n  capacity: 1\n", address.Port(), filepath.Join(dir, "work"), readerHub.URL, identityPath)
			if err := os.WriteFile(configPath, []byte(config), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := globalconfig.Read(configPath); err != nil {
				t.Fatal(err)
			}
			at := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
			writeLogLines(t, filepath.Join(dir, "detent.log"), testLogRecord(at, "INFO", "owned fixture log", name, "1", "owner/repository#1", 0, 0, ""))
			for _, test := range []struct {
				name          string
				held          bool
				projectStatus int
				revoked       bool
				expired       bool
				noProjects    bool
				wantErr       string
			}{
				{name: "without writer"},
				{name: "with writer", held: true},
				{name: "current grants replace cached grants", held: true, noProjects: true},
				{name: "missing project", held: true, projectStatus: http.StatusNotFound},
				{name: "rejected project", held: true, projectStatus: http.StatusUnauthorized, wantErr: "read runner project"},
				{name: "revoked project grant", held: true, projectStatus: http.StatusForbidden, wantErr: "read runner project"},
				{name: "revoked credential", held: true, revoked: true, wantErr: "read runner's allowed projects"},
				{name: "expired identity", held: true, expired: true, wantErr: "expired"},
			} {
				t.Run(test.name, func(t *testing.T) {
					projectStatus.Store(int32(test.projectStatus))
					identity := file.Identity
					if test.noProjects {
						identity.ProjectIDs = nil
					}
					identity.ExpiresAt = time.Now().Add(time.Minute)
					if test.expired {
						identity.ExpiresAt = time.Now().Add(-time.Minute)
					}
					hub.identity.Store(identity)
					credential := file.Credential
					if test.revoked {
						credential = "revoked_fixture_credential"
					}
					hub.credential.Store(credential)
					if test.held {
						lock, err := instancelock.Acquire(identityPath + ".lock")
						if err != nil {
							t.Fatal(err)
						}
						t.Cleanup(func() {
							if err := lock.Close(); err != nil {
								t.Error(err)
							}
						})
					}
					before := map[string][]byte{}
					beforeInfo := map[string]os.FileInfo{}
					for _, path := range []string{configPath, identityPath, identityPath + ".lock"} {
						data, err := os.ReadFile(path)
						if err != nil && !errors.Is(err, os.ErrNotExist) {
							t.Fatal(err)
						}
						before[path] = data
						if err == nil {
							info, err := os.Stat(path)
							if err != nil {
								t.Fatal(err)
							}
							beforeInfo[path] = info
						}
					}
					for _, command := range []string{"logs", "state", "exposure", "exposure project"} {
						t.Run(command, func(t *testing.T) {
							readsBefore := projectReads.Load()
							scans := 0
							cmd := NewRootCommand(t.Context())
							if strings.HasPrefix(command, "exposure") {
								cmd.RemoveCommand(findCommandByName(cmd, "exposure"))
								cmd.AddCommand(newExposureCommandWithDeps(&configPath, defaultOptions(), exposureDeps{
									loadWorkflow: func(project globalconfig.Project) (workflowconfig.Workflow, error) {
										if project.ID != string(projectID) {
											t.Fatalf("exposure loaded unauthorized project %s", project.ID)
										}
										workflow := workflowconfig.Workflow{Config: workflowconfig.Default()}
										workflow.Config.Tracker.Kind = workflowconfig.TrackerGitHub
										workflow.Config.Tracker.Repository = "owner/repository"
										workflow.Config.Codex.DeliverableElicitationAllowlist = []workflowconfig.DeliverableElicitationRule{{Repository: "example-org/public-project", Server: "codex_apps", Tool: "github.create_issue"}}
										return workflow, nil
									},
									repositoryInfo: func(_ context.Context, _ workflowconfig.Config, repository string) (ghconnector.RepositoryInfo, error) {
										if repository == "owner/repository" {
											return ghconnector.RepositoryInfo{Private: true, Visibility: "private"}, nil
										}
										return ghconnector.RepositoryInfo{Visibility: "public"}, nil
									},
									scan: func(_ context.Context, _ workflowconfig.Config, repository string, identifiers []string) ([]ghconnector.IssueExposure, error) {
										if repository != "example-org/public-project" || !containsString(identifiers, "owner/repository") {
											t.Fatalf("unexpected exposure scan: %s %v", repository, identifiers)
										}
										scans++
										return []ghconnector.IssueExposure{{Number: 42, MatchedIdentifier: "owner/repository"}}, nil
									},
								}))
							}
							var stdout, stderr bytes.Buffer
							cmd.SetOut(&stdout)
							cmd.SetErr(&stderr)
							args := []string{"--config", configPath, "--format", "json", strings.Fields(command)[0]}
							if command == "logs" {
								args = append(args, "--since", at.Format(time.RFC3339), "--until", at.Format(time.RFC3339), "--limit", "1", "--max-bytes", "1048576", "--output", "jsonl")
							}
							wantErr := test.wantErr
							if command == "exposure project" {
								args = append(args, "--project", string(projectID))
								if wantErr == "" && (test.noProjects || test.projectStatus == http.StatusNotFound) {
									wantErr = "not found"
								}
							}
							cmd.SetArgs(args)
							err := cmd.Execute()
							if wantErr != "" {
								if err == nil || !strings.Contains(err.Error(), wantErr) {
									t.Fatalf("authority refusal = %v, want %s", err, wantErr)
								}
								if stdout.Len() != 0 {
									t.Fatal("unauthorized diagnostic returned local data")
								}
							} else {
								if err != nil {
									t.Fatalf("read-only %s rejected: %v", command, err)
								}
								if test.noProjects {
									if projectReads.Load() != readsBefore {
										t.Fatal("diagnostic used a cached project grant")
									}
								} else if projectReads.Load() == readsBefore {
									t.Fatal("diagnostic did not resolve the enrolled project")
								}
								if command == "logs" && !strings.Contains(stdout.String(), "owned fixture log") {
									t.Fatal("log result omitted the fixture")
								}
								if command == "state" && !strings.Contains(stdout.String(), "generated_at") {
									t.Fatal("state result omitted the snapshot")
								}
							}
							if strings.HasPrefix(command, "exposure") {
								wantScans := 0
								if wantErr == "" && !test.noProjects && test.projectStatus == 0 {
									wantScans = 1
								}
								if scans != wantScans {
									t.Fatalf("exposure scans = %d, want %d", scans, wantScans)
								}
								if wantErr == "" {
									var report exposureReport
									if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
										t.Fatal(err)
									}
									if len(report.Findings) != wantScans || len(report.Warnings) != 0 {
										t.Fatalf("exposure report = %+v", report)
									}
									if wantScans == 1 && (report.Findings[0].SourceProject != string(projectID) || report.Findings[0].IssueNumber != 42) {
										t.Fatalf("exposure finding = %+v", report.Findings[0])
									}
								}
							}
							for _, secret := range []string{file.Credential, "fixture_service_token"} {
								if strings.Contains(stdout.String()+stderr.String()+fmt.Sprint(err), secret) {
									t.Fatal("diagnostic exposed a credential")
								}
							}
						})
					}
					for path, want := range before {
						got, err := os.ReadFile(path)
						if errors.Is(err, os.ErrNotExist) && want == nil {
							continue
						}
						if err != nil || want == nil || !bytes.Equal(got, want) {
							t.Errorf("diagnostic changed owned file %s: %v", filepath.Base(path), err)
						}
						info, err := os.Stat(path)
						previous := beforeInfo[path]
						if err != nil || previous == nil || !os.SameFile(previous, info) || info.Mode() != previous.Mode() || !info.ModTime().Equal(previous.ModTime()) {
							t.Errorf("diagnostic rewrote owned file %s: %v", filepath.Base(path), err)
						}
					}
					if test.held {
						inspection, err := instancelock.Inspect(identityPath + ".lock")
						if err != nil || inspection.Status != instancelock.StatusHeld {
							t.Fatalf("diagnostic lost the identity writer lock: %v", err)
						}
					}
					if _, err := os.Stat(filepath.Join(dir, "work")); !errors.Is(err, os.ErrNotExist) {
						t.Fatalf("diagnostic prepared a checkout: %v", err)
					}
				})
			}
		})
	}
}

func TestParseLogsFilter(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name        string
		values      logsCommandOptions
		wantSince   time.Time
		wantUntil   time.Time
		wantLevel   bool
		wantErrText string
	}{
		{
			name:      "default bounded window",
			values:    logsCommandOptions{Limit: defaultLogsLimit, MaxBytes: defaultLogsMaxBytes, Output: logsOutputJSON},
			wantSince: now.Add(-defaultLogsWindow),
			wantUntil: now,
		},
		{
			name: "normalizes explicit boundaries to UTC",
			values: logsCommandOptions{
				Since: "2026-08-07T06:00:00-05:00", Until: "2026-08-07T07:00:00-05:00",
				Level: "warning", Limit: 2, MaxBytes: 1024, Output: logsOutputJSONL,
			},
			wantSince: time.Date(2026, time.August, 7, 11, 0, 0, 0, time.UTC),
			wantUntil: time.Date(2026, time.August, 7, 12, 0, 0, 0, time.UTC),
			wantLevel: true,
		},
		{name: "rejects zero limit", values: logsCommandOptions{MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--limit must be greater than 0"},
		{name: "rejects zero byte bound", values: logsCommandOptions{Limit: 1, Output: logsOutputJSON}, wantErrText: "--max-bytes must be greater than 0"},
		{name: "rejects output", values: logsCommandOptions{Limit: 1, MaxBytes: 1, Output: "pretty"}, wantErrText: "--output must be json or jsonl"},
		{name: "rejects start syntax", values: logsCommandOptions{Since: "yesterday", Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--since must be an RFC3339 timestamp"},
		{name: "rejects end before start", values: logsCommandOptions{Since: now.Format(time.RFC3339), Until: now.Add(-time.Second).Format(time.RFC3339), Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--since must be before or equal to --until"},
		{name: "rejects level", values: logsCommandOptions{Level: "notice", Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--level must be debug, info, warn, or error"},
		{name: "rejects negative attempt", values: logsCommandOptions{WorkAttemptID: -1, Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--work-attempt-id must be greater than or equal to 0"},
		{name: "rejects negative session", values: logsCommandOptions{DetentSessionID: -1, Limit: 1, MaxBytes: 1, Output: logsOutputJSON}, wantErrText: "--detent-session-id must be greater than or equal to 0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseLogsFilter(tt.values, now)
			if tt.wantErrText != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrText) {
					t.Fatalf("parseLogsFilter() error = %v, want containing %q", err, tt.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLogsFilter() error = %v", err)
			}
			if !got.Since.Equal(tt.wantSince) || !got.Until.Equal(tt.wantUntil) {
				t.Fatalf("window = %s..%s, want %s..%s", got.Since, got.Until, tt.wantSince, tt.wantUntil)
			}
			if got.MinimumLevelSet != tt.wantLevel {
				t.Fatalf("MinimumLevelSet = %t, want %t", got.MinimumLevelSet, tt.wantLevel)
			}
		})
	}
}

func TestFilterLogsCombinesFiltersAndOrdersLevels(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "detent.log")
	base := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	records := []string{
		testLogRecord(base, "DEBUG", "debug", "alpha", "1", "owner/repo#1", 10, 20, "provider-a"),
		testLogRecord(base.Add(time.Minute), "INFO", "info", "alpha", "1", "owner/repo#1", 10, 20, "provider-a"),
		testLogRecord(base.Add(2*time.Minute), "WARN", "warn", "alpha", "1", "owner/repo#1", 10, 20, "provider-a"),
		testLogRecord(base.Add(3*time.Minute), "ERROR", "error", "beta", "2", "owner/repo#2", 11, 21, "provider-b"),
	}
	writeLogLines(t, path, records...)
	tests := []struct {
		name   string
		filter logsFilter
		want   []string
	}{
		{
			name:   "UTC boundaries are inclusive",
			filter: testLogsFilter(base.Add(time.Minute), base.Add(2*time.Minute)),
			want:   []string{"info", "warn"},
		},
		{
			name: "canonical filters combine conjunctively",
			filter: func() logsFilter {
				filter := testLogsFilter(base, base.Add(4*time.Minute))
				filter.ProjectID = "alpha"
				filter.IssueID = "1"
				filter.IssueIdentifier = "owner/repo#1"
				filter.WorkAttemptID = 10
				filter.DetentSessionID = 20
				filter.ProviderSession = "provider-a"
				filter.MinimumLevel, filter.MinimumLevelSet, _ = parseLogsLevel("warn")
				return filter
			}(),
			want: []string{"warn"},
		},
		{
			name: "project scope is exact",
			filter: func() logsFilter {
				filter := testLogsFilter(base, base.Add(4*time.Minute))
				filter.ProjectID = "beta"
				return filter
			}(),
			want: []string{"error"},
		},
	}
	for _, level := range []struct {
		name string
		want []string
	}{
		{name: "debug", want: []string{"debug", "info", "warn", "error"}},
		{name: "info", want: []string{"info", "warn", "error"}},
		{name: "warn", want: []string{"warn", "error"}},
		{name: "error", want: []string{"error"}},
	} {
		filter := testLogsFilter(base, base.Add(4*time.Minute))
		filter.MinimumLevel, filter.MinimumLevelSet, _ = parseLogsLevel(level.name)
		tests = append(tests, struct {
			name   string
			filter logsFilter
			want   []string
		}{name: "minimum level " + level.name, filter: filter, want: level.want})
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			result, diagnostics, err := filterLogs(path, 0, tt.filter)
			if err != nil {
				t.Fatalf("filterLogs() error = %v", err)
			}
			if len(diagnostics) != 0 {
				t.Fatalf("diagnostics = %#v, want none", diagnostics)
			}
			if got := logMessages(t, result.Records); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("messages = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestFilterLogsReadsRotationAndReportsMalformedRecordsSafely(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "detent.log")
	base := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	writeLogLines(t, path+".2", testLogRecord(base, "INFO", "oldest", "alpha", "1", "owner/repo#1", 0, 0, ""))
	active := testLogRecord(base.Add(time.Minute), "INFO", "newest", "alpha", "1", "owner/repo#1", 0, 0, "") + "\n" +
		`{"time":"2026-08-07T10:02:00Z","secret":"DO-NOT-ECHO",}` + "\n" +
		`{"time":"2026-08-07T10:02:30Z","level":"INFO","project_id":42,"secret":"WRONG-TYPE"}` + "\n" +
		`{"time":"2026-08-07T10:03:00Z","secret":"ALSO-SECRET"`
	if err := os.WriteFile(path, []byte(active), 0o600); err != nil {
		t.Fatal(err)
	}

	filter := testLogsFilter(base.Add(-time.Hour), base.Add(time.Hour))
	filter.ProjectID = "alpha"
	result, diagnostics, err := filterLogs(path, 2, filter)
	if err != nil {
		t.Fatalf("filterLogs() error = %v", err)
	}
	if got, want := logMessages(t, result.Records), []string{"oldest", "newest"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("messages = %v, want %v", got, want)
	}
	if result.Summary.MalformedRecords != 3 {
		t.Fatalf("MalformedRecords = %d, want 3", result.Summary.MalformedRecords)
	}
	if got, want := []string{diagnostics[0].Code, diagnostics[1].Code, diagnostics[2].Code}, []string{"malformed_json", "invalid_project_id", "partial_final_line"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("diagnostic codes = %v, want %v", got, want)
	}
	var stderr bytes.Buffer
	if err := writeLogsDiagnostics(&stderr, diagnostics, result.Summary); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "DO-NOT-ECHO") || strings.Contains(stderr.String(), "WRONG-TYPE") || strings.Contains(stderr.String(), "ALSO-SECRET") {
		t.Fatalf("diagnostics leaked record contents: %s", stderr.String())
	}
}

func TestOpenLogsSnapshotRetriesConcurrentRotationAndDeduplicates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "detent.log")
	base := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	writeLogLines(t, path+".1", testLogRecord(base, "INFO", "backup", "alpha", "1", "", 0, 0, ""))
	writeLogLines(t, path, testLogRecord(base.Add(time.Minute), "INFO", "active", "alpha", "1", "", 0, 0, ""))
	attempts := 0
	files, err := openLogsSnapshotWithHook(logsSnapshotPaths(path, 2), func(attempt int) error {
		attempts++
		if attempt > 0 {
			return nil
		}
		if err := rotateLogFiles(path, 2); err != nil {
			return err
		}
		writeLogLines(t, path, testLogRecord(base.Add(2*time.Minute), "INFO", "after rotation", "alpha", "1", "", 0, 0, ""))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeLogsSnapshot(files); err != nil {
			t.Errorf("closeLogsSnapshot() error = %v", err)
		}
	})
	if attempts != 2 {
		t.Fatalf("snapshot attempts = %d, want 2", attempts)
	}
	result, diagnostics := filterLogsSnapshot(files, testLogsFilter(base.Add(-time.Hour), base.Add(time.Hour)))
	if len(diagnostics) != 0 {
		t.Fatalf("diagnostics = %#v, want none", diagnostics)
	}
	if got, want := logMessages(t, result.Records), []string{"backup", "active", "after rotation"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot messages = %v, want %v", got, want)
	}

	linked := filepath.Join(dir, "linked.log")
	writeLogLines(t, linked, testLogRecord(base, "INFO", "once", "alpha", "1", "", 0, 0, ""))
	if err := os.Link(linked, linked+".1"); err != nil {
		t.Fatal(err)
	}
	deduplicated, err := openLogsSnapshot(linked, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := closeLogsSnapshot(deduplicated); err != nil {
			t.Errorf("closeLogsSnapshot() error = %v", err)
		}
	})
	if len(deduplicated) != 1 {
		t.Fatalf("snapshot file count = %d, want 1", len(deduplicated))
	}
}

func TestFilterLogsEnforcesByteAndRecordBounds(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name         string
		lines        []string
		filter       logsFilter
		wantMessages []string
		wantMatched  int
		wantReason   string
	}{
		{
			name: "byte tail starts on record boundary",
			lines: []string{
				testLogRecord(base, "INFO", "old", "alpha", "1", "", 0, 0, ""),
				testLogRecord(base.Add(time.Minute), "INFO", "new", "alpha", "1", "", 0, 0, ""),
			},
			filter: func() logsFilter {
				filter := testLogsFilter(base.Add(-time.Hour), base.Add(time.Hour))
				filter.MaxBytes = int64(len(testLogRecord(base.Add(time.Minute), "INFO", "new", "alpha", "1", "", 0, 0, "")) + 1)
				return filter
			}(),
			wantMessages: []string{"new"},
			wantMatched:  1,
			wantReason:   logsTruncationBytes,
		},
		{
			name: "record limit retains latest matches in order",
			lines: []string{
				testLogRecord(base, "INFO", "one", "alpha", "1", "", 0, 0, ""),
				testLogRecord(base.Add(time.Minute), "INFO", "two", "alpha", "1", "", 0, 0, ""),
				testLogRecord(base.Add(2*time.Minute), "INFO", "three", "alpha", "1", "", 0, 0, ""),
			},
			filter: func() logsFilter {
				filter := testLogsFilter(base.Add(-time.Hour), base.Add(time.Hour))
				filter.Limit = 2
				return filter
			}(),
			wantMessages: []string{"two", "three"},
			wantMatched:  3,
			wantReason:   logsTruncationRecords,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "detent.log")
			writeLogLines(t, path, tt.lines...)
			result, diagnostics, err := filterLogs(path, 0, tt.filter)
			if err != nil {
				t.Fatal(err)
			}
			if got := logMessages(t, result.Records); !reflect.DeepEqual(got, tt.wantMessages) {
				t.Fatalf("messages = %v, want %v", got, tt.wantMessages)
			}
			if result.Summary.MatchedRecords != tt.wantMatched || result.Summary.ReturnedRecords != len(tt.wantMessages) {
				t.Fatalf("summary counts = matched %d returned %d", result.Summary.MatchedRecords, result.Summary.ReturnedRecords)
			}
			if !result.Summary.Truncated || !reflect.DeepEqual(result.Summary.TruncationReasons, []string{tt.wantReason}) {
				t.Fatalf("truncation = %t %v, want %q", result.Summary.Truncated, result.Summary.TruncationReasons, tt.wantReason)
			}
			var stderr bytes.Buffer
			if err := writeLogsDiagnostics(&stderr, diagnostics, result.Summary); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(stderr.String(), `"code":"output_truncated"`) {
				t.Fatalf("stderr does not signal truncation: %s", stderr.String())
			}
		})
	}
}

func TestWriteLogsOutputShapes(t *testing.T) {
	t.Parallel()
	record := json.RawMessage(`{"time":"2026-08-07T10:00:00Z","level":"INFO","msg":"ready"}`)
	result := logsResult{Records: []json.RawMessage{record}, Summary: logsSummary{ReturnedRecords: 1}}
	tests := []struct {
		name   string
		output string
		check  func(*testing.T, []byte)
	}{
		{
			name:   "JSON envelope",
			output: logsOutputJSON,
			check: func(t *testing.T, raw []byte) {
				var decoded struct {
					Records []json.RawMessage `json:"records"`
					Summary logsSummary       `json:"summary"`
				}
				if err := json.Unmarshal(raw, &decoded); err != nil {
					t.Fatal(err)
				}
				if len(decoded.Records) != 1 || decoded.Summary.ReturnedRecords != 1 {
					t.Fatalf("decoded output = %#v", decoded)
				}
			},
		},
		{
			name:   "JSONL raw records",
			output: logsOutputJSONL,
			check: func(t *testing.T, raw []byte) {
				if got, want := string(raw), string(record)+"\n"; got != want {
					t.Fatalf("output = %q, want %q", got, want)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			if err := writeLogsOutput(&output, tt.output, result); err != nil {
				t.Fatal(err)
			}
			tt.check(t, output.Bytes())
		})
	}
}

func TestLogsSourceUsesRuntimeResolutionAndOverride(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "global.yaml")
	backups := 3
	opts := defaultOptions()
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{Path: configPath, Rule: globalconfig.PathRuleFlag}, nil
	}
	opts.readDoctor = func(string) (globalconfig.Config, error) {
		return globalconfig.Config{Path: configPath, LogMaxBackups: &backups}, nil
	}
	path, gotBackups, err := resolveLogsSource("ignored", opts)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "detent.log"); path != want || gotBackups != backups {
		t.Fatalf("source = %q, %d, want %q, %d", path, gotBackups, want, backups)
	}

	override := filepath.Join(dir, "runtime", "operator.jsonl")
	path, gotBackups = logsSourceFromBoot(BootConfig{
		RuntimeDBPath:  filepath.Join(dir, "runtime", "state.db"),
		RuntimeLogPath: override,
		Runtime:        RuntimeSettings{LogMaxBackups: RuntimeIntValue{Value: 7}},
	})
	if path != override || gotBackups != 7 {
		t.Fatalf("overridden source = %q, %d, want %q, 7", path, gotBackups, override)
	}
}

func TestFilterLogsExplainsNoFileLoggingModes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "detent.log")
	_, _, err := filterLogs(path, 5, testLogsFilter(time.Unix(0, 0), time.Now().Add(time.Hour)))
	if err == nil {
		t.Fatal("filterLogs() error = nil")
	}
	for _, want := range []string{"runtime log file is unavailable", "terminal-dashboard mode", "headless and text modes"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error missing %q: %v", want, err)
		}
	}
}

func TestFilterLogsBoundsMalformedDiagnostics(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "detent.log")
	lines := make([]string, logsMaxDiagnostics+2)
	for index := range lines {
		lines[index] = `{"secret":"NEVER-ECHO",}`
	}
	writeLogLines(t, path, lines...)
	result, diagnostics, err := filterLogs(path, 0, testLogsFilter(time.Unix(0, 0), time.Now().Add(time.Hour)))
	if err != nil {
		t.Fatal(err)
	}
	if len(diagnostics) != logsMaxDiagnostics || result.Summary.SuppressedDetails != 2 {
		t.Fatalf("diagnostics = %d, suppressed = %d, want %d and 2", len(diagnostics), result.Summary.SuppressedDetails, logsMaxDiagnostics)
	}
	var stderr bytes.Buffer
	if err := writeLogsDiagnostics(&stderr, diagnostics, result.Summary); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stderr.String(), "NEVER-ECHO") || !strings.Contains(stderr.String(), `"code":"diagnostics_suppressed","count":2`) {
		t.Fatalf("bounded diagnostics = %s", stderr.String())
	}
}

func TestLogsCommandReadsResolvedRuntimeLog(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "global.yaml")
	path := filepath.Join(dir, "detent.log")
	at := time.Date(2026, time.August, 7, 10, 0, 0, 0, time.UTC)
	writeLogLines(t, path, testLogRecord(at, "INFO", "command", "alpha", "1", "owner/repo#1", 0, 0, ""))
	opts := defaultOptions()
	opts.resolvePath = func(string) (globalconfig.PathResolution, error) {
		return globalconfig.PathResolution{Path: configPath, Rule: globalconfig.PathRuleFlag}, nil
	}
	opts.readDoctor = func(string) (globalconfig.Config, error) {
		return globalconfig.Config{Path: configPath}, nil
	}
	cmd := NewRootCommand(t.Context(), func(configured *options) { *configured = opts })
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{
		"logs",
		"--project-id", "alpha",
		"--since", at.Format(time.RFC3339),
		"--until", at.Format(time.RFC3339),
		"--output", "jsonl",
	})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if !strings.Contains(stdout.String(), `"msg":"command"`) {
		t.Fatalf("stdout = %s", stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %s, want empty", stderr.String())
	}
}

func testLogsFilter(since time.Time, until time.Time) logsFilter {
	return logsFilter{
		Since:    since.UTC(),
		Until:    until.UTC(),
		Limit:    100,
		MaxBytes: 1 << 20,
	}
}

func testLogRecord(at time.Time, level string, message string, projectID string, issueID string, issueIdentifier string, workAttemptID int64, detentSessionID int64, providerSessionID string) string {
	fields := map[string]any{
		"time":                at.Format(time.RFC3339Nano),
		"level":               level,
		"msg":                 message,
		"project_id":          projectID,
		"issue_id":            issueID,
		"issue_identifier":    issueIdentifier,
		"work_attempt_id":     workAttemptID,
		"detent_session_id":   detentSessionID,
		"provider_session_id": providerSessionID,
	}
	raw, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

func writeLogLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func logMessages(t *testing.T, records []json.RawMessage) []string {
	t.Helper()
	messages := make([]string, 0, len(records))
	for _, record := range records {
		var fields map[string]any
		if err := json.Unmarshal(record, &fields); err != nil {
			t.Fatal(err)
		}
		message, ok := fields["msg"].(string)
		if !ok {
			t.Fatalf("record has no string msg: %s", record)
		}
		messages = append(messages, message)
	}
	return messages
}

func TestLogsErrorsRemainClassified(t *testing.T) {
	t.Parallel()
	_, err := parseLogsFilter(logsCommandOptions{Limit: -1, MaxBytes: 1, Output: logsOutputJSON}, time.Now())
	if !errors.Is(err, ErrValidation) {
		t.Fatalf("error = %v, want ErrValidation", err)
	}
	if got := ClassifyError(err).ExitCode; got != 3 {
		t.Fatalf("exit code = %d, want 3", got)
	}
}
