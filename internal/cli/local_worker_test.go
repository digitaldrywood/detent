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

	"github.com/digitaldrywood/detent/internal/isolation"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	configwatcher "github.com/digitaldrywood/detent/internal/config/watcher"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gobudget"
	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/hubclient/nativechange"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/orchestrator"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/procgroup"
	"github.com/digitaldrywood/detent/internal/procstart"
	projectpkg "github.com/digitaldrywood/detent/internal/project"
	provenance "github.com/digitaldrywood/detent/internal/releaseprovenance"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/runnersession"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/testutil/projectfixture"
	"github.com/digitaldrywood/detent/internal/tracker"
	detentupdate "github.com/digitaldrywood/detent/internal/update"
	"github.com/digitaldrywood/detent/internal/workspace"
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
		var lifecycle sync.Once
		var peer *runnerpkg.SSHPeer
		sources := &runnerpkg.SSHExecutionSources{}
		peer = runnerpkg.NewSSHPeer(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, method string, args []json.RawMessage) (any, error) {
			<-ready
			if strings.HasPrefix(method, "source.") {
				return sources.Handle(ctx, method, args)
			}
			var started bool
			lifecycle.Do(func() { started = true })
			if !started {
				return nil, errors.New("fixture worker already started")
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
		lifecycle.Do(func() { close(finished) })
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

const localWorkerWorkflow = "---\ntracker:\n  kind: hub_native\nworker:\n  github_token: fixture-token\ngate:\n  run: true\n---\nRun the fixture.\n"

type localWorkerPolicyScheduling struct{ *hubclient.Scheduler }

func (s localWorkerPolicyScheduling) FetchCandidateIssues(context.Context, orchestrator.SchedulingRequest) ([]connector.Issue, error) {
	return nil, nil
}

type localWorkerPolicyWatcher struct{ updates <-chan configwatcher.Update }

func (w localWorkerPolicyWatcher) Watch(context.Context) (<-chan configwatcher.Update, error) {
	return w.updates, nil
}

func localWorkerHub(t *testing.T, observe ...func(*http.Request)) (*hubclient.Scheduler, *clihangNativeClient, *hubclient.NativeConnector, config.Workflow, func(string) connector.Issue, func()) {
	return localWorkerHubFixture(t, nil, observe...)
}

func localWorkerHubFixture(t *testing.T, expose func(http.Handler, tracker.OrganizationID, tracker.ProjectID, string, config.Workflow), observe ...func(*http.Request)) (*hubclient.Scheduler, *clihangNativeClient, *hubclient.NativeConnector, config.Workflow, func(string) connector.Issue, func()) {
	t.Helper()
	return localWorkerHubWithTransport(t, expose, nil, observe...)
}

func localWorkerHubWithTransport(t *testing.T, expose func(http.Handler, tracker.OrganizationID, tracker.ProjectID, string, config.Workflow), intercept func(http.Handler, *http.Request) (*http.Response, error), observe ...func(*http.Request)) (*hubclient.Scheduler, *clihangNativeClient, *hubclient.NativeConnector, config.Workflow, func(string) connector.Issue, func()) {
	t.Helper()
	const adminToken = "fixture-admin"
	var contentMu sync.Mutex
	contentObjects := make(map[string][]byte)
	content := func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := request.Reference.Validate(request.Organization); err != nil {
			return nil, err
		}
		contentMu.Lock()
		defer contentMu.Unlock()
		if request.Write {
			contentObjects[request.Reference.Key] = bytes.Clone(request.Data)
			return nil, nil
		}
		data, found := contentObjects[request.Reference.Key]
		if !found {
			return nil, errors.New("archive unavailable")
		}
		return bytes.Clone(data), nil
	}
	service, revoke := clihangPostgresHub(t, adminToken, content)
	var refusals []string
	t.Cleanup(func() {
		if t.Failed() {
			contentMu.Lock()
			defer contentMu.Unlock()
			t.Logf("fixture Hub refusals: %v", refusals)
		}
	})
	httpClient := &http.Client{Transport: workerHubTransport(func(request *http.Request) (*http.Response, error) {
		for _, observer := range observe {
			observer(request)
		}
		if intercept != nil {
			return intercept(service.Handler(), request)
		}
		recorder := httptest.NewRecorder()
		service.Handler().ServeHTTP(recorder, request)
		if recorder.Code >= http.StatusBadRequest {
			contentMu.Lock()
			refusals = append(refusals, fmt.Sprintf("%s %s: %d worker=%t", request.Method, request.URL.Path, recorder.Code, request.Header.Get("Authorization") == "Bearer "+adminToken+"-worker"))
			contentMu.Unlock()
		}
		return recorder.Result(), nil
	})}
	organization := tracker.OrganizationID("org_example")
	project := tracker.NativeProject{ID: "prj_org_example"}
	client, err := hubclient.New(hubclient.Config{URL: "http://worker-hub.test", TokenSource: func() string { return adminToken }, HTTPClient: httpClient})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native(organization, project.ID)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := config.ParseWorkflow([]byte(localWorkerWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := projectpkg.ResolvePolicy(globalconfig.Project{ID: "local"}, workflow)
	if err != nil {
		t.Fatal(err)
	}
	workflow.Config.Policy = descriptor
	if _, err := native.ApproveProjectPolicy(t.Context(), policy.Change{Policy: descriptor}); err != nil {
		t.Fatal(err)
	}
	workerClient, err := hubclient.New(hubclient.Config{URL: "http://worker-hub.test", TokenSource: func() string { return adminToken + "-worker" }, HTTPClient: httpClient})
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
		Machine: hubclient.Machine{ID: clihangBinding(organization).MachineID, Hostname: "fixture-host", Capacity: 2, Version: "1.0.0", BackendIsolation: isolation.Report{"codex": {string(isolation.NativeTrusted), string(isolation.Sandbox)}}}, HeartbeatInterval: time.Second, LeaseTTL: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if expose != nil {
		expose(service.Handler(), organization, project.ID, adminToken+"-worker", workflow)
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
	projectPages := func() {
		for {
			ctx := (hubmaintenance.Budget{Items: 1, Bytes: hubmaintenance.ByteLimit}).Context(t.Context(), hubmaintenance.Tenant{Organization: string(organization), Generation: 1})
			worked, err := service.MaintainWorkItemPage(ctx, organization)
			if err != nil {
				t.Fatal(err)
			}
			if !worked {
				return
			}
		}
	}
	return scheduler, &clihangNativeClient{NativeClient: workerNative, admin: native, projectPages: projectPages}, connection, workflow, create, revoke
}

func testLocalWorkerScenario(t *testing.T, scenario string) {
	t.Helper()
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	defer cancel()
	scheduler, native, connection, workflow, create, revoke := localWorkerHub(t)
	scratch := t.TempDir()
	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, scratch)
	}
	workflow.Config.Workspace.Root = t.TempDir()
	descriptor := workflow.Config.Policy
	artifacts, publish, cacheRoot := localWorkerArtifacts(t)
	publishRelease := publish
	publish = func(version string) {
		publishRelease(version)
		artifact, err := artifacts.Acquire(ctx, workerProtocolPreflight)
		if err != nil {
			t.Fatal(err)
		}
		if err := artifact.Release(); err != nil {
			t.Fatal(err)
		}
	}
	publish("1.0.0")
	workerClient, _ := scheduler.NativeClient("local")
	built, err := buildRunner(workflow, "local", t.TempDir(), globalconfig.Memory{}, gobudget.Budget{}, newHubBudgetSessionStore(workerClient, "local"), slog.New(slog.NewTextHandler(io.Discard, nil)), serviceapi.Connection{})
	if err != nil {
		t.Fatal(err)
	}
	run := built.(*sshRunner)
	run.artifacts = artifacts
	policyUpdates := make(chan configwatcher.Update)
	root := t.TempDir()
	path := filepath.Join(root, "WORKFLOW.md")
	if err := os.WriteFile(path, []byte(localWorkerWorkflow), 0o600); err != nil {
		t.Fatal(err)
	}
	selected, err := projectfixture.New(projectpkg.Config{Project: globalconfig.Project{ID: "local", Workdir: root, Workflow: path}, Workflow: workflow}, projectpkg.Dependencies{
		Runner: run, Scheduling: localWorkerPolicyScheduling{scheduler}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), WorkflowReconcileInterval: time.Hour,
		WorkflowWatcherFactory: func(string) (projectpkg.WorkflowWatcher, error) { return localWorkerPolicyWatcher{policyUpdates}, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := selected.Close(); err != nil {
			t.Error(err)
		}
	})
	if err := selected.Start(ctx); err != nil {
		t.Fatal(err)
	}
	policyEvents, err := selected.Events().Subscribe(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer policyEvents.Close()
	applyPolicy := func(prompt string) {
		t.Helper()
		candidate, err := config.ParseWorkflow([]byte(strings.ReplaceAll(localWorkerWorkflow, "Run the fixture.", prompt)))
		if err != nil {
			t.Fatal(err)
		}
		replacement, err := projectpkg.ResolvePolicy(selected.Config(), candidate)
		if err != nil {
			t.Fatal(err)
		}
		if replacement.SameAuthoredInputs(descriptor) {
			t.Fatal("policy publication did not change authored inputs")
		}
		if _, err := native.ApproveProjectPolicy(ctx, policy.Change{ExpectedID: descriptor.ID, Policy: replacement}); err != nil {
			t.Fatal(err)
		}
		select {
		case policyUpdates <- configwatcher.Update{Path: path, Workflow: candidate, At: time.Now().UTC()}:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		for {
			select {
			case event := <-policyEvents.C():
				if event.Kind != projectpkg.EventWorkflowReloaded {
					continue
				}
				if selected.Workflow().Config.Policy.ID != replacement.ID {
					t.Fatalf("project reload did not adopt published policy: %+v", selected.WorkflowSourceStatus())
				}
				descriptor = replacement
				return
			case <-ctx.Done():
				t.Fatalf("policy reload waited for active work: %+v", selected.WorkflowSourceStatus())
			}
		}
	}
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
		evidence, err := clihangRuntimeEvidence(ctx, native, tracker.NativeWorkItemID(issue.ID))
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
	if scenario != "authority" {
		a := start("Compile parser", "1.0.0", runnerpkg.RunModeImplement)
		if scenario == "coexistence" {
			publish("1.0.1")
			b := start("Render invoice", "1.0.1", runnerpkg.RunModeImplement)
			finish(b, false)
			applyPolicy("Apply the current review instructions.")
			continuePhase(b)
			c := start("Archive inventory", "1.0.1", runnerpkg.RunModeImplement)
			if alive, err := procgroup.Alive(a.identity); err != nil || !alive {
				t.Fatalf("compatible update interrupted A: %t %v", alive, err)
			}
			finish(c, true)
			publish("1.0.2")
			applyPolicy("Apply the updated validation instructions.")
			d := start("Validate manifest", "1.0.2", runnerpkg.RunModeImplement)
			finish(d, false)
			publish("1.0.3")
			continuePhase(d)
			e := start("Index catalog", "1.0.3", runnerpkg.RunModeImplement)
			finish(e, true)
		} else {
			publish("1.0.3")
		}
		if scenario == "failures" {
			for _, failure := range []struct{ name, mode string }{{"Parse grammar", "startup_exit"}, {"Export report", "delivery_exit"}, {"Resolve translations", "policy_mismatch"}} {
				failed := start(failure.name, "1.0.3", failure.mode)
				failed.finish()
				select {
				case completion := <-failed.result:
					if completion.Err == nil {
						t.Fatal("unexpected worker exit appeared successful")
					}
					if decision, ok := runnerpkg.FailureDecision(completion.Err); !ok || decision.Outcome != "instance" {
						t.Fatalf("%s failure lost caller attribution: %+v", failure.mode, decision)
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
				evidence, err := clihangRuntimeEvidence(ctx, native, tracker.NativeWorkItemID(failed.issue.ID))
				if err != nil || evidence.Attempt == nil || evidence.Attempt.Outcome == "succeeded" || evidence.Attempt.TerminalFailure == nil || evidence.Attempt.TerminalFailure.Attribution != "instance" {
					t.Fatalf("worker failure lost instance attribution: %+v %v", evidence.Attempt, err)
				}
				if alive, err := procgroup.Alive(a.identity); err != nil || !alive {
					t.Fatalf("failed worker interrupted A: %t %v", alive, err)
				}
			}
		}
		if _, err := os.Stat(a.binary); err != nil {
			t.Fatalf("old artifact removed before final owner exited: %v", err)
		}
		finish(a, true)
		if _, err := os.Stat(a.binary); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("obsolete artifact retained after final owner exited: %v", err)
		}
		return
	}
	publish("1.0.3")
	lost := start("Summarize documents", "1.0.3", runnerpkg.RunModeImplement)
	runnerClient, ok := scheduler.NativeClient("local")
	if !ok {
		t.Fatal("runner client is unavailable")
	}
	if err := runnerClient.Release(ctx, lost.request.Execution.Recovery().Lease, "cancelled"); err != nil {
		t.Fatal(err)
	}
	if _, err := scheduler.RenewClaim(ctx, lost.issue.ID, time.Now()); !errors.Is(err, orchestrator.ErrSchedulingClaimLost) {
		t.Fatalf("released claim renewal = %v", err)
	}
	select {
	case result := <-lost.result:
		if result.Err == nil {
			t.Fatal("claim-lost worker completed successfully")
		}
	case <-ctx.Done():
		t.Fatal("claim loss did not stop active execution")
	}
	if alive, err := procgroup.Alive(lost.identity); err != nil || alive {
		t.Fatalf("claim-lost worker survived cleanup: %t %v", alive, err)
	}
	if scheduler.RunExecution(lost.issue.ID) == lost.request.Execution {
		t.Fatal("claim-lost worker retained its native claim")
	}
	lostItem, err := native.Issue(ctx, tracker.NativeWorkItemID(lost.issue.ID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := native.Transition(ctx, lostItem.WorkItemID, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "lost-worker-done"}, ExpectedRevision: lostItem.Revision, State: "Done", Reason: "user_requested"}); err != nil {
		t.Fatal(err)
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

type launchReply struct {
	io.ReadCloser
	atClose func()
}

func (r launchReply) Close() error {
	r.atClose()
	return r.ReadCloser.Close()
}

func testFencedLaunchAcknowledgement(t *testing.T, scenario string) {
	ctx, cancel := context.WithCancel(testenv.WaitContext(t, t.Context()))
	defer cancel()
	var events []tracker.NativeRunEvent
	var registration runnersession.Mutation
	var registeredSessionID int64
	var acked, reconnectReads int
	var inventory func() []runnerauth.AttemptWorker
	var reconnectSource *hubclient.NativeConnector
	var mu sync.Mutex
	sent := make(chan struct{}, 2)
	intercept := func(handler http.Handler, request *http.Request) (*http.Response, error) {
		var event tracker.NativeRunEvent
		var sessionStart runnersession.Mutation
		if request.URL.Query().Has("attempt_worker_after") {
			mu.Lock()
			reconnectReads++
			mu.Unlock()
		}
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/events") {
			raw, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			request.Body = io.NopCloser(bytes.NewReader(raw))
			if err := json.Unmarshal(raw, &event); err != nil {
				return nil, err
			}
		}
		if request.Method == http.MethodPost && strings.HasSuffix(request.URL.Path, "/runner-sessions") {
			raw, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			request.Body = io.NopCloser(bytes.NewReader(raw))
			if err := json.Unmarshal(raw, &sessionStart); err != nil {
				return nil, err
			}
		}
		if event.Worker != nil {
			mu.Lock()
			events = append(events, event)
			n := len(events)
			mu.Unlock()
			switch scenario {
			case "before_registration":
				cancel()
				return nil, io.ErrUnexpectedEOF
			case "unreachable", "preparation_unreachable":
				return nil, io.ErrUnexpectedEOF
			case "after_commit_before_reply", "after_ack_before_release", "lost_reply":
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				resp := recorder.Result()
				if resp.StatusCode != http.StatusOK {
					return resp, nil
				}
				mu.Lock()
				acked++
				mu.Unlock()
				switch scenario {
				case "after_commit_before_reply":
					cancel()
					return nil, errors.Join(io.ErrUnexpectedEOF, resp.Body.Close())
				case "after_ack_before_release":
					resp.Body = launchReply{ReadCloser: resp.Body, atClose: cancel}
				case "lost_reply":
					if n == 1 {
						return nil, errors.Join(io.ErrUnexpectedEOF, resp.Body.Close())
					}
				}
				return resp, nil
			}
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if sessionStart.Start != nil && sessionStart.Worker != nil && sessionStart.Worker.CleanupPath != "" && recorder.Code == http.StatusOK {
			var snapshot runnersession.Snapshot
			if err := json.Unmarshal(recorder.Body.Bytes(), &snapshot); err != nil {
				return nil, err
			}
			mu.Lock()
			registration, registeredSessionID = sessionStart, snapshot.ID
			mu.Unlock()
		}
		if event.Type != "" {
			t.Logf("%s sequence=%d launch=%t retirement=%t status=%d", event.Type, event.Data.Sequence, event.Worker != nil, event.WorkerCleanup != nil, recorder.Code)
		}
		return recorder.Result(), nil
	}
	scheduler, native, _, workflow, create, _ := localWorkerHubWithTransport(t, func(handler http.Handler, organization tracker.OrganizationID, project tracker.ProjectID, token string, _ config.Workflow) {
		inventory = func() []runnerauth.AttemptWorker {
			path := fmt.Sprintf("/api/v2/organizations/%s/projects/%s/machines/%s/heartbeat?attempt_worker_after=", organization, project, clihangBinding(organization).MachineID)
			request := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}")).WithContext(context.WithoutCancel(ctx))
			request.Header.Set("Authorization", "Bearer "+token)
			request.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			var response runnerauth.AttemptWorkerInventory
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != http.StatusOK || response.Page == nil {
				t.Fatalf("read typed inventory: status=%d error=%v", recorder.Code, err)
			}
			return response.Page.Workers
		}
		if scenario == "recovered_live" || scenario == "recovered_exited" {
			path := filepath.Join(t.TempDir(), "private", "runner.json")
			file, err := runnerauth.Initialize(path, "https://worker-hub.test")
			if err != nil {
				t.Fatal(err)
			}
			file.Identity.OrganizationID = organization
			file.Identity.ExpiresAt = time.Now().Add(runnerauth.CredentialTTL)
			if err := runnerauth.Save(path, file); err != nil {
				t.Fatal(err)
			}
			client, err := hubclient.New(hubclient.Config{URL: file.HubURL, IdentityFile: path, HTTPClient: &http.Client{
				Transport: workerHubTransport(func(request *http.Request) (*http.Response, error) {
					request.Header.Set("Authorization", "Bearer "+token)
					if !request.URL.Query().Has("attempt_worker_after") {
						return intercept(handler, request)
					}
					request.URL.Path = strings.Replace(request.URL.Path, string(file.Identity.MachineID), string(clihangBinding(organization).MachineID), 1)
					resp, err := intercept(handler, request)
					if err != nil || resp.StatusCode != http.StatusOK {
						return resp, err
					}
					var wire runnerauth.AttemptWorkerInventory
					err = errors.Join(json.NewDecoder(resp.Body).Decode(&wire), resp.Body.Close())
					if err != nil {
						return nil, err
					}
					wire.RunnerID = file.Identity.RunnerID
					raw, err := json.Marshal(wire)
					resp.Body = io.NopCloser(bytes.NewReader(raw))
					return resp, err
				}),
			}})
			if err != nil {
				t.Fatal(err)
			}
			native, err := client.Native(organization, project)
			if err != nil {
				t.Fatal(err)
			}
			reconnectSource, err = hubclient.NewNativeConnector(native)
			if err != nil {
				t.Fatal(err)
			}
		}
	}, intercept)
	workspaceRoot := t.TempDir()
	workflow.Config.Workspace.Root = workspaceRoot
	artifacts, publish, _ := localWorkerArtifacts(t)
	publish("1.0.0")
	t.Setenv("GOCOVERDIR", t.TempDir())
	artifact, err := artifacts.Acquire(ctx, workerProtocolPreflight)
	if err != nil {
		t.Fatal(err)
	}
	if err := artifact.Release(); err != nil {
		t.Fatal(err)
	}

	workerClient, _ := scheduler.NativeClient("local")
	built, err := buildRunner(workflow, "local", t.TempDir(), globalconfig.Memory{}, gobudget.Budget{}, newHubBudgetSessionStore(workerClient, "local"), slog.New(slog.NewTextHandler(io.Discard, nil)), serviceapi.Connection{})
	if err != nil {
		t.Fatal(err)
	}
	run := built.(*sshRunner)
	run.artifacts = artifacts
	issue := create("Check launch acknowledgement")
	for _, candidate := range testLocalWorkerCandidates(t, ctx, scheduler, workflow) {
		if candidate.ID == issue.ID {
			claim, err := scheduler.AdoptClaim(ctx, candidate, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			issue = claim.Issue
			break
		}
	}
	execution := scheduler.RunExecution(issue.ID)
	if execution == nil {
		t.Fatal("fixture did not claim issue")
	}
	t.Cleanup(func() { _ = scheduler.ReleaseClaim(context.WithoutCancel(ctx), issue.ID, "completed") })
	request := runnerpkg.RunRequest{
		ProjectID: "local", Issue: issue, Policy: workflow.Config.Policy, Execution: execution,
		Mode: runnerpkg.RunModeImplement, WorkAttemptID: 1, Generation: 1, DeferExecutionFinish: true,
		ProgressProbe: func(context.Context) (string, error) {
			sent <- struct{}{}
			return "continue", nil
		},
	}
	var outcome runnerpkg.RunResult
	var runErr error
	if strings.HasPrefix(scenario, "preparation_") {
		prepared := false
		request.PrepareRun = func(prepareCtx context.Context, request *runnerpkg.RunRequest) error {
			prepared = true
			if workers := inventory(); len(workers) != 1 {
				return errors.New("preparation ran before the attempt worker was recorded")
			}
			request.PriorAttempt.Source = "prepared request"
			return nil
		}
		supervisor, err := runnerpkg.NewSupervisor(run, runnerpkg.SupervisorConfig{})
		if err != nil {
			t.Fatal(err)
		}
		completion := supervisor.Run(ctx, request)
		if scenario == "preparation_unreachable" {
			if prepared || completion.Err == nil || len(sent) != 0 || len(events) == 0 {
				t.Fatalf("unacknowledged preparation: prepared=%t error=%v effects=%d launch writes=%d", prepared, completion.Err, len(sent), len(events))
			}
			return
		}
		if !prepared || completion.Err != nil || completion.Request.PriorAttempt.Source != "prepared request" ||
			completion.Result.FinalState != runnerpkg.FinalStateCompleted || len(sent) != 1 ||
			len(events) != 1 {
			t.Fatalf("acknowledged preparation: prepared=%t error=%v final state=%q effects=%d launch writes=%d", prepared, completion.Err, completion.Result.FinalState, len(sent), len(events))
		}
		return
	}
	if scenario == "before_registration" || scenario == "lost_reply" {
		commandDir := t.TempDir()
		t.Setenv("PATH", commandDir+string(os.PathListSeparator)+os.Getenv("PATH"))
		for name, args := range map[string]string{"gh": "auth token", "git": "config --get remote.origin.url"} {
			body := fmt.Sprintf("#!/bin/sh\ncase \"$*\" in *'%s') : > \"$0.started\";; esac\necho fixture-value\n", args)
			if err := os.WriteFile(filepath.Join(commandDir, name), []byte(body), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		workflow.Config.Worker.GitHubToken = "gh"
		if err := run.UpdateWorkflowChecked(workflow); err != nil {
			t.Fatal(err)
		}
		defer func() {
			for _, name := range []string{"gh", "git"} {
				_, err := os.Stat(filepath.Join(commandDir, name+".started"))
				if scenario == "before_registration" && !errors.Is(err, os.ErrNotExist) || scenario == "lost_reply" && err != nil {
					t.Errorf("%s launch marker after %s: %v", name, scenario, err)
				}
			}
		}()
	}
	outcome, runErr = run.Run(ctx, request)
	if scenario == "recovered_live" || scenario == "recovered_exited" {
		if runErr != nil || len(events) != 1 || len(sent) != 1 {
			t.Fatalf("initial launch: error=%v writes=%d effects=%d", runErr, len(events), len(sent))
		}
		if scenario == "recovered_exited" {
			if err := execution.(runnerpkg.WorkerLifetimeExecution).CloseWorkers(ctx); err != nil {
				t.Fatal(err)
			}
		}
		claim, ok := scheduler.ExecutionClaim(issue.ID)
		if !ok {
			t.Fatal("same-fence claim was removed")
		}
		claim.Recovery, runErr = workerClient.Recovery(ctx, tracker.NativeWorkItemID(issue.ID))
		if runErr != nil {
			t.Fatal(runErr)
		}
		claim.Source = reconnectSource
		request.Execution = nativechange.NewExecution(scheduler, claim)
		defer request.Execution.(runnerpkg.WorkerLifetimeExecution).CloseWorkers(context.WithoutCancel(ctx))
		_, runErr := run.Run(ctx, request)
		if reconnectReads != 1 {
			t.Fatalf("same-fence launch used %d fresh inventory reads (error=%v)", reconnectReads, runErr)
		}
		if scenario == "recovered_live" {
			if !errors.Is(runErr, runnerpkg.ErrExecutionAuthorityUnavailable) || len(events) != 1 || len(sent) != 1 {
				t.Fatalf("recovery replaced a live owner: error=%v writes=%d effects=%d", runErr, len(events), len(sent))
			}
		} else if runErr != nil || len(events) != 2 || len(sent) != 2 || events[0].Data.FencingToken != events[1].Data.FencingToken {
			t.Fatalf("recovery did not replace the proven-exited owner: error=%v writes=%d effects=%d", runErr, len(events), len(sent))
		}
		return
	}
	if scenario == "second_launch" || scenario == "overlapping" {
		if runErr != nil || outcome.FinalState != runnerpkg.FinalStateCompleted || len(sent) != 1 {
			t.Fatalf("first launch: outcome=%s error=%v releases=%d", outcome.FinalState, runErr, len(sent))
		}
		before := inventory()
		if len(before) != 1 || before[0].Worker != *events[0].Worker {
			t.Fatalf("primary inventory before continuation: %+v", before)
		}
		if scenario == "second_launch" {
			if err := execution.(runnerpkg.WorkerLifetimeExecution).CloseWorkers(ctx); err != nil {
				t.Fatal(err)
			}
		}
		request.Mode = runnerpkg.RunModeMerge
		second, err := run.Run(ctx, request)
		if err != nil || second.FinalState != runnerpkg.FinalStateCompleted || len(sent) != 2 {
			t.Fatalf("continued launch: outcome=%s error=%v releases=%d", second.FinalState, err, len(sent))
		}
		after := inventory()
		if len(after) != 1 {
			t.Fatalf("continued inventory: %+v", after)
		}
		if scenario == "second_launch" {
			if len(events) != 2 || events[1].Type != "run.observed" || events[1].Data.Sequence <= events[0].Data.Sequence ||
				events[1].Data.AttemptID != events[0].Data.AttemptID || events[1].Data.FencingToken != events[0].Data.FencingToken ||
				after[0].Worker == before[0].Worker || after[0].Worker != *events[1].Worker {
				t.Fatalf("second launch failed to replace a retired worker: %+v", after)
			}
			return
		}
		sessionWorker := registration.Worker
		if len(events) != 1 || after[0] != before[0] || sessionWorker == nil || registeredSessionID <= 0 ||
			registration.LeaseID != events[0].Data.LeaseID || registration.FencingToken != events[0].Data.FencingToken ||
			sessionWorker.PID == int(before[0].Worker.PID) || filepath.Dir(filepath.Dir(sessionWorker.CleanupPath)) != bestEffortTestPath(t, workspaceRoot) {
			t.Fatalf("overlap lost primary ownership or fenced session registration: owners=%+v session=%+v", after, sessionWorker)
		}
		var snapshot runnersession.Snapshot
		path := fmt.Sprintf("%s/runner-sessions/%d", workerClient.Scope(), registeredSessionID)
		err = workerClient.Transport().Request(ctx, http.MethodGet, path, nil, &snapshot)
		if err != nil || snapshot.Worker == nil || *snapshot.Worker != *sessionWorker {
			t.Fatalf("overlapping worker was not durable on its session: %+v %v", snapshot.Worker, err)
		}
		if matches, err := procstart.Matches(sessionWorker.PID, sessionWorker.StartKey); err != nil || !matches {
			t.Fatalf("session birth identity mismatch: %t %v", matches, err)
		}
		return
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 || events[0].Type != "run.started" || events[0].Data.Sequence != 1 || !events[0].Worker.Valid() {
		t.Fatalf("first event did not register the birth identity: %+v, execution error %v", events, runErr)
	}
	key := events[0].Worker.StartKey
	if !strings.HasPrefix(key, runtime.GOOS+":") {
		t.Fatalf("first event start key %q does not come from procstart", key)
	}
	if scenario == "lost_reply" {
		if runErr != nil || outcome.FinalState != runnerpkg.FinalStateCompleted || len(sent) != 1 || len(events) != 2 || acked != 2 {
			t.Fatalf("replayed launch: outcome=%s error=%v releases=%d writes=%d accepted=%d", outcome.FinalState, runErr, len(sent), len(events), acked)
		}
		first, _ := json.Marshal(events[0])
		replay, _ := json.Marshal(events[1])
		if !bytes.Equal(first, replay) {
			t.Fatal("launch retry changed the fenced event")
		}
	} else {
		if runErr == nil || len(sent) != 0 {
			t.Fatalf("bootstrap released without permission: error=%v releases=%d", runErr, len(sent))
		}
		if alive, err := procgroup.Alive(procgroup.Identity{PID: int(events[0].Worker.PID), GroupID: int(events[0].Worker.PGID), StartedAt: events[0].Worker.StartedAt}); err != nil || alive {
			t.Fatalf("unreleased bootstrap survived EOF: alive=%t error=%v", alive, err)
		}
	}
	attempts, err := native.Attempts(context.WithoutCancel(ctx), tracker.NativeWorkItemID(issue.ID), "")
	if err != nil {
		t.Fatal(err)
	}
	registered := len(attempts.Items) == 1 && attempts.Items[0].Runtime != nil && attempts.Items[0].Runtime.WorkerLaunch != nil
	if want := scenario != "before_registration" && scenario != "unreachable"; registered != want {
		t.Fatalf("durable launch present=%t want=%t", registered, want)
	}
	workers := inventory()
	if registered && (len(workers) != 1 || workers[0].Worker != *events[0].Worker) || !registered && len(workers) != 0 {
		t.Fatalf("typed worker inventory after launch: %+v", workers)
	}
	roots, err := os.ReadDir(workspaceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(roots) != 1 || !strings.HasSuffix(roots[0].Name(), "-execution") {
		t.Fatalf("workspace root contains more than private bootstrap staging: %v", roots)
	}
}

func testLocalWorkerCandidates(t *testing.T, ctx context.Context, scheduler *hubclient.Scheduler, workflow config.Workflow) []connector.Issue {
	t.Helper()
	candidates, err := scheduler.FetchCandidateIssues(ctx, orchestrator.SchedulingRequest{Policy: workflow.Config.Policy, ProjectID: "local", WorkflowStates: []string{"Todo", "In Progress"}})
	if err != nil {
		t.Fatal(err)
	}
	return candidates
}

func bestEffortTestPath(t *testing.T, path string) string {
	t.Helper()
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func TestReview1973NativeRunRefusesUnownedRoute(t *testing.T) {
	for _, host := range []string{"local", "example-worker"} {
		t.Run(host, func(t *testing.T) {
			backend, err := workspace.NewFilesystem(workspace.FilesystemOptions{Root: t.TempDir(), OutputRoot: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			run, err := runnerpkg.NewRunner(runnerpkg.Dependencies{Workspace: backend, AgentBackend: stubConversationBackend{}})
			if err != nil {
				t.Fatal(err)
			}
			execution := &review1973UnownedExecution{}
			supervisor, err := runnerpkg.NewSupervisor(&sshRunner{Runner: run}, runnerpkg.SupervisorConfig{})
			if err != nil {
				t.Fatal(err)
			}
			prepared := false
			outcome := supervisor.Run(t.Context(), runnerpkg.RunRequest{
				Issue:      connector.Issue{ID: "example-issue"},
				WorkerHost: host, Execution: execution,
				PrepareRun: func(context.Context, *runnerpkg.RunRequest) error {
					prepared = true
					return nil
				},
			})
			if !errors.Is(outcome.Err, runnerpkg.ErrExecutionAuthorityUnavailable) || prepared || execution.guarded {
				t.Fatalf("unowned route reached preparation or execution: error=%v prepared=%t guarded=%t", outcome.Err, prepared, execution.guarded)
			}
		})
	}
}

type review1973UnownedExecution struct {
	runnerpkg.Execution
	runnerpkg.AttemptWorkerExecution
	guarded bool
}

func (e *review1973UnownedExecution) Guard(ctx context.Context) (context.Context, func(), error) {
	e.guarded = true
	return ctx, func() {}, runnerpkg.ErrExecutionAuthorityUnavailable
}
