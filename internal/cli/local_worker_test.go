package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
)

func TestLocalWorkerProcess(t *testing.T) {
	version := os.Getenv("DETENT_EXECUTION_FIXTURE_VERSION")
	if version == "" {
		return
	}
	commit := os.Getenv("DETENT_EXECUTION_FIXTURE_COMMIT")
	switch os.Args[len(os.Args)-1] {
	case "version":
		fmt.Fprintf(os.Stdout, "version: v%s\ncommit: %s\n", version, commit)
	case SSHProbeArgument:
		fmt.Fprintln(os.Stdout, runnerpkg.SSHProtocolVersion)
	case SSHWorkerArgument:
		if os.Getenv("DETENT_EXECUTION_REAL_WORKER") == "true" {
			if err := RunSSHWorker(context.Background(), os.Stdin, os.Stdout, slog.New(slog.NewTextHandler(os.Stderr, nil))); err != nil {
				os.Exit(1)
			}
			os.Exit(0)
		}
		ready, finished := make(chan struct{}), make(chan struct{})
		var peer *runnerpkg.SSHPeer
		sources := &runnerpkg.SSHExecutionSources{}
		peer = runnerpkg.NewSSHPeer(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
			<-ready
			if strings.HasPrefix(method, "source.") {
				return sources.Handle(ctx, method, args)
			}
			defer close(finished)
			var boot sshBootstrap
			if method != "run" || len(args) != 1 {
				return nil, errors.New("invalid worker bootstrap")
			}
			if err := json.Unmarshal(args[0], &boot); err != nil {
				return nil, err
			}
			if boot.Run.Request.Mode == "startup_exit" {
				os.Exit(7)
			}
			boot.Run.Sources = sources
			request := boot.Run.Bind(ctx, peer)
			if err := request.Execution.Start(ctx, tracker.NativeExecutionIdentity{Role: runnerpkg.RoleCode, Backend: "codex", Model: "test"}); err != nil {
				return nil, err
			}
			var release string
			if err := peer.Call(ctx, "progress", &release); err != nil {
				return nil, err
			}
			if completion, ok := request.Execution.(runnerpkg.CompletionExecution); ok {
				if err := completion.PrepareFinish(ctx, "succeeded", "Worker finished", nil); err != nil {
					return nil, err
				}
			}
			if boot.Run.Request.Mode == "delivery_exit" {
				os.Exit(8)
			}
			return runnerpkg.NewSSHRunResponse(runnerpkg.RunResult{FinalState: runnerpkg.FinalStateCompleted, FinalMessage: boot.Policy.ID + "/" + version}, nil), nil
		})
		close(ready)
		<-peer.Context().Done()
		<-finished
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

type processReleases struct {
	mu        sync.Mutex
	releases  []detentupdate.Release
	downloads map[string][]byte
}

func (f *processReleases) ListReleases(context.Context) ([]detentupdate.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]detentupdate.Release(nil), f.releases...), nil
}

func (f *processReleases) Download(_ context.Context, url string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, ok := f.downloads[url]
	if !ok {
		return nil, errors.New("fixture artifact unavailable")
	}
	return data, nil
}

func (f *processReleases) GetRelease(_ context.Context, tag string) (detentupdate.Release, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, release := range f.releases {
		if release.TagName == tag {
			return release, nil
		}
	}
	return detentupdate.Release{}, errors.New("fixture release unavailable")
}

func localWorkerArtifacts(t *testing.T, actual ...bool) (*detentupdate.WorkerArtifacts, func(string), string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture := &processReleases{downloads: make(map[string][]byte)}
	service := detentupdate.NewService(detentupdate.Config{CurrentVersion: "1.0.0", GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, Client: fixture,
		ChecksumSignatureVerifier: func(_ context.Context, data, signature []byte) error {
			if !ed25519.Verify(public, data, signature) {
				return errors.New("invalid signature")
			}
			return nil
		},
		BinaryVerifier: func(ctx context.Context, path string) (string, error) {
			output, err := exec.CommandContext(ctx, path, "version").Output()
			return string(output), err
		},
	})
	root := t.TempDir()
	artifacts, err := detentupdate.NewWorkerArtifacts(service, root)
	if err != nil {
		t.Fatal(err)
	}
	return artifacts, func(version string) {
		t.Helper()
		fixture.mu.Lock()
		defer fixture.mu.Unlock()
		commit := fmt.Sprintf("%x", sha256.Sum256([]byte(version)))[:40]
		quoted := "'" + strings.ReplaceAll(executable, "'", "'\"'\"'") + "'"
		realWorker := len(actual) > 0 && actual[0]
		data := []byte(fmt.Sprintf("#!/bin/sh\nGORACE=\"${GORACE:+$GORACE }atexit_sleep_ms=0\" DETENT_EXECUTION_REAL_WORKER=%t DETENT_EXECUTION_FIXTURE_VERSION=%s DETENT_EXECUTION_FIXTURE_COMMIT=%s exec %s -test.run=^TestLocalWorkerProcess$ -- \"$@\"\n", realWorker, version, commit, quoted))
		var archive bytes.Buffer
		gz := gzip.NewWriter(&archive)
		tarball := tar.NewWriter(gz)
		if err := tarball.WriteHeader(&tar.Header{Name: "detent", Mode: 0o755, Size: int64(len(data))}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarball.Write(data); err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(tarball.Close(), gz.Close()); err != nil {
			t.Fatal(err)
		}
		manifest, err := provenance.Marshal(provenance.Manifest{Schema: provenance.Schema, Repository: "digitaldrywood/detent", Tag: "v" + version, Commit: commit,
			Checks: []provenance.Check{{Name: "CI", Status: "completed", Conclusion: "success", CheckRunID: 1}}})
		if err != nil {
			t.Fatal(err)
		}
		archiveName := fmt.Sprintf("detent_%s_%s_%s.tar.gz", version, runtime.GOOS, runtime.GOARCH)
		checksumName := "detent_" + version + "_checksums.txt"
		manifestName := "detent_release_provenance.json"
		checksums := fmt.Appendf(nil, "%x  %s\n%x  %s\n", sha256.Sum256(archive.Bytes()), archiveName, sha256.Sum256(manifest), manifestName)
		release := detentupdate.Release{TagName: "v" + version}
		for name, data := range map[string][]byte{archiveName: archive.Bytes(), checksumName: checksums, checksumName + ".minisig": ed25519.Sign(private, checksums), manifestName: manifest} {
			url := "https://releases.example.test/" + version + "/" + name
			fixture.downloads[url] = data
			release.Assets = append(release.Assets, detentupdate.Asset{Name: name, BrowserDownloadURL: url})
		}
		fixture.releases = append(fixture.releases, release)
	}, root
}

type workerHubTransport func(*http.Request) (*http.Response, error)

func (f workerHubTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func localWorkerHub(t *testing.T) (*hubclient.Scheduler, *hubclient.NativeClient, *hubclient.NativeConnector, policy.Descriptor, func(string) connector.Issue, func()) {
	t.Helper()
	const adminToken = "fixture-admin"
	service, err := hubserver.Open(t.Context(), hubserver.Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), InitialAdminToken: []byte(adminToken)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	httpClient := &http.Client{Transport: workerHubTransport(func(request *http.Request) (*http.Response, error) {
		recorder := httptest.NewRecorder()
		service.Handler().ServeHTTP(recorder, request)
		return recorder.Result(), nil
	})}
	request := func(method, path string, body, result any) {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), method, "http://worker-hub.test"+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		response, err := httpClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		if response.StatusCode >= 300 {
			data, _ := io.ReadAll(response.Body)
			t.Fatalf("fixture %s %s: %d %s", method, path, response.StatusCode, data)
		}
		if result != nil {
			if err := json.NewDecoder(response.Body).Decode(result); err != nil {
				t.Fatal(err)
			}
		}
	}
	var organizations tracker.Page[struct {
		ID tracker.OrganizationID `json:"organization_id"`
	}]
	request(http.MethodGet, "/api/v2/organizations", nil, &organizations)
	organization := organizations.Items[0].ID
	var project tracker.NativeProject
	states := []tracker.NativeState{{Name: "Todo", Dispatchable: true, Transitions: []string{"In Progress", "Done"}}, {Name: "In Progress", Dispatchable: true, Transitions: []string{"Done"}}, {Name: "Done", Terminal: true}}
	request(http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects", map[string]any{"name": "worker-lifecycle", "idempotency_key": "worker-lifecycle", "states": states, "require_dependencies": false}, &project)
	client, err := hubclient.New(hubclient.Config{URL: "http://worker-hub.test", TokenSource: func() string { return adminToken }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	descriptor := policy.Descriptor{SourceRevision: strings.Repeat("a", 40), SourceDigest: policy.Digest([]byte("source")), ConfigDigest: policy.Digest([]byte("config")),
		Gates: policy.Gates{Kind: "command", PlanReview: "human", PlanStopDigest: policy.Digest([]byte("Plan Review")), AutomatedReview: "optional", MergeMethod: "squash"}}.WithID()
	if _, err := native.ApproveProjectPolicy(t.Context(), policy.Change{Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	var token struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	request(http.MethodPost, "/api/v1/tokens", map[string]string{"name": "fixture-worker", "scope": "worker"}, &token)
	request(http.MethodPost, "/api/v2/tokens/"+token.ID+"/grants", map[string]any{"organization_id": organization, "project_id": project.ID}, nil)
	workerClient, err := hubclient.New(hubclient.Config{URL: "http://worker-hub.test", TokenSource: func() string { return token.Token }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	workerNative, err := workerClient.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	connection, err := hubclient.NewNativeConnector(workerNative)
	if err != nil {
		t.Fatal(err)
	}
	scheduler, err := hubclient.NewScheduler(workerClient, hubclient.SchedulerConfig{OrganizationID: organization, NativeProjects: map[string]tracker.ProjectID{"local": project.ID},
		Machine: hubclient.Machine{ID: "fixture-machine", Hostname: "fixture-host", Capacity: 2, Version: "1.0.0"}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	create := func(name string) connector.Issue {
		t.Helper()
		draft := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: policy.Digest([]byte(name))}, Title: name, State: "Todo",
			Body: name + ".\n## Acceptance criteria\nFinish " + name + ".\n## Must not break\nOther work.\n## How we know it worked\nCheck result."}
		raw, err := json.Marshal(draft)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v2/organizations/"+string(organization)+"/projects/"+string(project.ID)+"/work-items", bytes.NewReader(raw)).WithContext(t.Context())
		req.Header.Set("Authorization", "Bearer "+adminToken)
		req.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		service.Handler().ServeHTTP(recorder, req)
		var issue tracker.NativeIssue
		if recorder.Code == http.StatusConflict {
			var challenge struct {
				Token   string `json:"challenge_token"`
				Matches []struct {
					Number int `json:"number"`
				} `json:"possible_duplicates"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &challenge); err != nil || challenge.Token == "" {
				t.Fatalf("fixture duplicate challenge: %s %v", recorder.Body, err)
			}
			draft.ChallengeToken = challenge.Token
			for _, match := range challenge.Matches {
				draft.NotDuplicateOf = append(draft.NotDuplicateOf, match.Number)
			}
			issue, err = native.CreateIssue(t.Context(), draft)
		} else if recorder.Code < 300 {
			err = json.Unmarshal(recorder.Body.Bytes(), &issue)
		} else {
			t.Fatalf("fixture issue: %d %s", recorder.Code, recorder.Body)
		}
		if err != nil {
			t.Fatal(err)
		}
		return connector.Issue{ID: string(issue.WorkItemID), Title: name, State: issue.State}
	}
	return scheduler, native, connection, descriptor, create, func() { request(http.MethodDelete, "/api/v1/tokens/"+token.ID, nil, nil) }
}

func TestLocalWorkerReleaseAndPolicyCoexistence(t *testing.T) {
	if testing.Short() {
		t.Skip("process and Hub lifecycle integration")
	}
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("Linux and macOS execution fixture")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	scheduler, native, connection, descriptor, create, revoke := localWorkerHub(t)
	artifacts, publish, cacheRoot := localWorkerArtifacts(t)
	publish("1.0.0")
	cfg := config.Default()
	cfg.Policy = descriptor
	cfg.Worker.GitHubToken = "fixture-token"
	cfg.Tracker.APIKey = "fixture-token"
	workflow := config.Workflow{Config: cfg, Prompt: "Run the fixture."}
	built, err := buildRunner(workflow, "local", t.TempDir(), globalconfig.Memory{}, gobudget.Budget{}, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), serviceapi.Connection{})
	if err != nil {
		t.Fatal(err)
	}
	run := built.(*sshRunner)
	run.artifacts = artifacts
	type worker struct {
		issue    connector.Issue
		identity procgroup.Identity
		finish   func()
		result   chan runnerpkg.Completion
		expected string
		binary   string
		request  runnerpkg.RunRequest
	}
	var attempt int64
	start := func(name, version, mode string) worker {
		t.Helper()
		issue := create(name)
		candidates, err := scheduler.FetchCandidateIssues(ctx, orchestrator.SchedulingRequest{Policy: descriptor, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, candidate := range candidates {
			if candidate.ID == issue.ID {
				claim, err := scheduler.AdoptClaim(ctx, candidate, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				issue, found = claim.Issue, true
			}
		}
		if !found {
			t.Fatalf("second slot unavailable for %s: %+v", name, candidates)
		}
		execution := scheduler.RunExecution(issue.ID)
		ready, finish := make(chan struct{}), make(chan struct{})
		result := make(chan runnerpkg.Completion, 1)
		attempt++
		request := runnerpkg.RunRequest{ProjectID: "local", Issue: issue, Policy: descriptor, Execution: execution, Mode: mode, WorkAttemptID: attempt, Generation: 1, DeferExecutionFinish: true,
			ProgressProbe: func(ctx context.Context) (string, error) {
				close(ready)
				select {
				case <-finish:
					return "finish", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}}
		if mode == "policy_mismatch" {
			request.Policy.ID = "policy_missing"
		}
		var finishOnce sync.Once
		stop := func() { finishOnce.Do(func() { close(finish) }) }
		done := make(chan struct{})
		go func() {
			defer close(done)
			outcome, err := run.Run(ctx, request)
			result <- runnerpkg.Completion{Result: outcome, Err: err}
		}()
		t.Cleanup(func() {
			stop()
			<-done
			if err := scheduler.ReleaseClaim(context.WithoutCancel(ctx), issue.ID, "completed"); err != nil && !errors.Is(err, runnerpkg.ErrExecutionAuthorityUnavailable) {
				t.Error(err)
			}
		})
		select {
		case <-ready:
		case completed := <-result:
			if (mode == "startup_exit" || mode == "policy_mismatch") && completed.Err != nil {
				result <- completed
				return worker{issue: issue, finish: stop, result: result}
			}
			t.Fatalf("worker never became ready: %v", completed.Err)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		evidence, err := native.RuntimeEvidence(ctx, tracker.NativeWorkItemID(issue.ID), "")
		if err != nil || evidence.Attempt == nil || evidence.Attempt.Runtime == nil || evidence.Attempt.Runtime.WorkerLaunch == nil {
			t.Fatalf("launch identity missing: %+v %v", evidence, err)
		}
		launch := evidence.Attempt.Runtime.WorkerLaunch
		if launch.ExecutionVersion != version || launch.ControlVersion != "1.0.0" || launch.PolicyID != descriptor.ID || launch.ExecutionSHA256 == "" {
			t.Fatalf("wrong captured launch: %+v", launch)
		}
		return worker{issue: issue, identity: procgroup.Identity{PID: launch.PID, GroupID: launch.GroupID, StartedAt: launch.ProcessStart}, finish: stop, result: result,
			expected: descriptor.ID + "/" + version, binary: filepath.Join(cacheRoot, "worker-"+launch.ExecutionSHA256, "detent"), request: request}
	}
	release := func(w worker) {
		t.Helper()
		if err := connection.UpdateIssueState(ctx, w.issue.ID, "Done"); err != nil {
			t.Fatal(err)
		}
		if err := scheduler.ReleaseClaim(ctx, w.issue.ID, "completed"); err != nil {
			t.Fatal(err)
		}
		if alive, err := procgroup.Alive(w.identity); err != nil || alive {
			t.Fatalf("worker survived finalization: alive=%t err=%v", alive, err)
		}
	}
	finish := func(w worker, releaseClaim bool) {
		t.Helper()
		w.finish()
		select {
		case completed := <-w.result:
			if completed.Err != nil || completed.Result.FinalState != runnerpkg.FinalStateCompleted || completed.Result.FinalMessage != w.expected {
				t.Fatalf("worker failed or changed its captured identity: %v %s", completed.Err, completed.Result.FinalMessage)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if alive, err := procgroup.Alive(w.identity); err != nil || !alive {
			t.Fatalf("worker exited before claim finalization: alive=%t err=%v", alive, err)
		}
		if releaseClaim {
			release(w)
		}
	}
	continuePhase := func(w worker) {
		t.Helper()
		request := w.request
		request.Mode = runnerpkg.RunModeMerge
		request.ProgressProbe = func(context.Context) (string, error) { return "finish", nil }
		result, err := run.Run(ctx, request)
		if err != nil || result.FinalMessage != w.expected {
			t.Fatalf("continuation changed its captured release or policy: %s %v", result.FinalMessage, err)
		}
		release(w)
	}
	a := start("Compile parser", "1.0.0", runnerpkg.RunModeImplement)
	publish("1.0.1")
	b := start("Render invoice", "1.0.1", runnerpkg.RunModeImplement)
	finish(b, false)
	previous := descriptor
	descriptor.SourceRevision = strings.Repeat("b", 40)
	descriptor = descriptor.WithID()
	if _, err := native.ApproveProjectPolicy(ctx, policy.Change{ExpectedID: previous.ID, Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	workflow.Config.Policy = descriptor
	run.UpdateWorkflow(workflow)
	continuePhase(b)
	c := start("Archive inventory", "1.0.1", runnerpkg.RunModeImplement)
	if alive, err := procgroup.Alive(a.identity); err != nil || !alive {
		t.Fatalf("compatible update interrupted A: %t %v", alive, err)
	}
	finish(c, true)
	publish("1.0.2")
	previous = descriptor
	descriptor.SourceRevision = strings.Repeat("c", 40)
	descriptor = descriptor.WithID()
	if _, err := native.ApproveProjectPolicy(ctx, policy.Change{ExpectedID: previous.ID, Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	workflow.Config.Policy = descriptor
	run.UpdateWorkflow(workflow)
	d := start("Validate manifest", "1.0.2", runnerpkg.RunModeImplement)
	finish(d, false)
	publish("1.0.3")
	continuePhase(d)
	e := start("Index catalog", "1.0.3", runnerpkg.RunModeImplement)
	finish(e, true)
	for _, failure := range []struct{ name, mode string }{{"Parse grammar", "startup_exit"}, {"Export report", "delivery_exit"}, {"Resolve translations", "policy_mismatch"}} {
		failed := start(failure.name, "1.0.3", failure.mode)
		failed.finish()
		select {
		case completion := <-failed.result:
			if completion.Err == nil {
				t.Fatal("unexpected worker exit appeared successful")
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if err := connection.UpdateIssueState(ctx, failed.issue.ID, "Done"); err != nil {
			t.Fatal(err)
		}
		if err := scheduler.ReleaseClaim(ctx, failed.issue.ID, "failed"); err != nil {
			t.Fatal(err)
		}
		evidence, err := native.RuntimeEvidence(ctx, tracker.NativeWorkItemID(failed.issue.ID), "")
		if err != nil || evidence.Attempt == nil || evidence.Attempt.Outcome == "succeeded" || evidence.Attempt.TerminalFailure == nil || evidence.Attempt.TerminalFailure.Attribution != "instance" {
			t.Fatalf("worker failure lost instance attribution: %+v %v", evidence.Attempt, err)
		}
		if alive, err := procgroup.Alive(a.identity); err != nil || !alive {
			t.Fatalf("failed worker interrupted A: %t %v", alive, err)
		}
	}
	if _, err := os.Stat(a.binary); err != nil {
		t.Fatalf("old artifact removed before final owner exited: %v", err)
	}
	finish(a, true)
	if _, err := os.Stat(a.binary); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("obsolete artifact retained after final owner exited: %v", err)
	}
	revoked := start("Classify documents", "1.0.3", runnerpkg.RunModeImplement)
	revoke()
	if _, err := scheduler.RenewClaim(ctx, revoked.issue.ID, time.Now()); err == nil {
		t.Fatal("captured policy concealed live credential revocation")
	}
	select {
	case result := <-revoked.result:
		if result.Err == nil {
			t.Fatal("revoked worker completed successfully")
		}
	case <-ctx.Done():
		t.Fatal("credential revocation did not stop active execution")
	}
	if alive, err := procgroup.Alive(revoked.identity); err != nil || alive {
		t.Fatalf("revoked worker survived cleanup: %t %v", alive, err)
	}
}
