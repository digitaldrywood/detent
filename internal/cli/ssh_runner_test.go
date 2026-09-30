package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/config"
	globalconfig "github.com/digitaldrywood/detent/internal/config/global"
	"github.com/digitaldrywood/detent/internal/connector"
	"github.com/digitaldrywood/detent/internal/gobudget"
	runnerpkg "github.com/digitaldrywood/detent/internal/runner"
	"github.com/digitaldrywood/detent/internal/serviceapi"
	"github.com/digitaldrywood/detent/internal/store"
)

func TestSSHServiceProxyKeepsCentralAuthority(t *testing.T) {
	central := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RequestURI() != "/workpad?issue=3239" || r.Header.Get("Authorization") != "Bearer project-scoped" {
			t.Errorf("unexpected service request: %s %v", r.URL, r.Header)
		}
		w.Header().Set("X-Owner", "central")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("recorded"))
	}))
	defer central.Close()
	r := &sshRunner{connection: serviceapi.Connection{Address: central.URL}}
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	owner := runnerpkg.NewSSHPeer(t.Context(), left, left, func(ctx context.Context, _ string, args []json.RawMessage) (any, error) {
		return r.forwardService(ctx, args)
	})
	defer owner.Close()
	worker := runnerpkg.NewSSHPeer(t.Context(), right, right, nil)
	defer worker.Close()
	address, closeProxy, err := serveSSHService(worker)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProxy()
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, "http://"+address+"/workpad?issue=3239", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer project-scoped")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != http.StatusAccepted || response.Header.Get("X-Owner") != "central" || string(body) != "recorded" {
		t.Fatalf("service response: %+v %q %v", response, body, err)
	}
}

func TestSSHProbeRequiresProvidedScratch(t *testing.T) {
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, "")
	}
	if SSHWorkerReady() {
		t.Fatal("host without scratch was available")
	}
}

func TestSSHHelperProcess(t *testing.T) {
	mode := os.Getenv("DETENT_SSH_TEST_HELPER")
	if mode == "" {
		return
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	switch mode {
	case "worker":
		err := RunSSHWorker(context.Background(), os.Stdin, os.Stdout, logger)
		_ = os.WriteFile(filepath.Join(os.Getenv("HOME"), "worker-finished"), []byte("finished"), 0o600)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	case "transport":
		executable, err := os.Executable()
		if err != nil {
			os.Exit(1)
		}
		cmd := exec.Command(executable, "-test.run=^TestSSHHelperProcess$")
		cmd.Env = append(os.Environ(), "DETENT_SSH_TEST_HELPER=worker")
		childInput, err := cmd.StdinPipe()
		if err != nil {
			os.Exit(1)
		}
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		go func() { _, _ = io.Copy(childInput, os.Stdin); _ = childInput.Close() }()
		if err := cmd.Run(); err != nil {
			os.Exit(1)
		}
	case "probe":
		if !SSHWorkerReady() {
			os.Exit(1)
		}
		fmt.Fprintln(os.Stdout, runnerpkg.SSHProtocolVersion)
	case "provider":
		if err := sshTestProvider(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
	os.Exit(0)
}

func sshTestProvider() error {
	decoder, encoder := json.NewDecoder(os.Stdin), json.NewEncoder(os.Stdout)
	for {
		var message struct {
			ID     json.RawMessage
			Method string
			Params struct {
				Cwd string `json:"cwd"`
			}
		}
		if err := decoder.Decode(&message); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		var result any
		switch message.Method {
		case "initialized":
			continue
		case "initialize":
			result = map[string]any{"userAgent": "isolated-ssh-fixture"}
		case "model/list":
			result = map[string]any{"data": []any{map[string]any{"id": "fixture", "model": "fixture", "isDefault": true, "supportedReasoningEfforts": []any{map[string]any{"reasoningEffort": "high"}}}}, "nextCursor": nil}
		case "config/read":
			result = map[string]any{"config": map[string]any{"model_provider": "openai"}}
		case "account/read":
			result = map[string]any{"account": map[string]any{"type": "chatgpt", "planType": "plus"}, "requiresOpenaiAuth": false}
		case "thread/start", "thread/resume":
			result = map[string]any{"thread": map[string]any{"id": "remote-thread"}}
		case "turn/start":
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": map[string]any{"turn": map[string]any{"id": "remote-turn"}}}); err != nil {
				return err
			}
			if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "method": "turn/started", "params": map[string]any{"threadId": "remote-thread", "turn": map[string]any{"id": "remote-turn", "status": "inProgress"}}}); err != nil {
				return err
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), "hold-worker")); err == nil {
				_, err = io.Copy(io.Discard, os.Stdin)
				return err
			}
			if err := os.WriteFile(filepath.Join(message.Params.Cwd, "feature.txt"), []byte("remote worker\n"), 0o644); err != nil {
				return err
			}
			for _, args := range [][]string{{"add", "feature.txt"}, {"-c", "user.name=SSH fixture", "-c", "user.email=ssh@example.test", "commit", "-m", "remote fixture"}, {"push", "origin", "HEAD"}} {
				cmd := exec.Command("git", args...)
				cmd.Dir = message.Params.Cwd
				if data, err := cmd.CombinedOutput(); err != nil {
					return fmt.Errorf("git fixture: %w: %s", err, data)
				}
			}
			gate := exec.Command("sh", "-c", os.Getenv("DETENT_SSH_TEST_GATE"))
			gate.Dir = message.Params.Cwd
			if err := gate.Run(); err != nil {
				return fmt.Errorf("remote gate: %w", err)
			}
			for _, notification := range []map[string]any{
				{"jsonrpc": "2.0", "method": "item/agentMessage/delta", "params": map[string]any{"threadId": "remote-thread", "turnId": "remote-turn", "itemId": "final", "delta": "Finished remote work."}},
				{"jsonrpc": "2.0", "method": "thread/tokenUsage/updated", "params": map[string]any{"threadId": "remote-thread", "turnId": "remote-turn", "tokenUsage": map[string]any{"total": map[string]any{"inputTokens": 10, "outputTokens": 5, "totalTokens": 15}}}},
				{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"threadId": "remote-thread", "turn": map[string]any{"id": "remote-turn", "status": "completed"}}},
			} {
				if err := encoder.Encode(notification); err != nil {
					return err
				}
			}
			continue
		case "turn/interrupt":
			result = map[string]any{}
		default:
			return fmt.Errorf("unexpected fixture method: %s", message.Method)
		}
		if err := encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result}); err != nil {
			return err
		}
	}
}

func TestSSHWorkerLifecycle(t *testing.T) { testSSHWorkerLifecycle(t, false) }

func TestSSHLocalTargetIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("local SSH integration requires a daemon")
	}
	testSSHWorkerLifecycle(t, true)
}

func testSSHWorkerLifecycle(t *testing.T, useSSH bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	root := t.TempDir()
	localHome, remoteHome := filepath.Join(root, "local"), filepath.Join(root, "remote")
	for _, path := range []string{localHome, remoteHome, filepath.Join(root, "bin")} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", localHome)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for _, name := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_CONFIG_COUNT"} {
		t.Setenv(name, "")
		if strings.HasPrefix(name, "GIT_") {
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	git := func(path string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = path
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	bare := filepath.Join(root, "origin.git")
	git(root, "init", "--bare", bare)
	source := filepath.Join(localHome, "projects", "repo")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	git(source, "init", "-b", "develop")
	if err := os.WriteFile(filepath.Join(source, "README.md"), []byte("fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(source, "add", "README.md")
	git(source, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "initial")
	git(source, "remote", "add", "origin", bare)
	git(source, "push", "origin", "develop")
	git(root, "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/develop")
	remoteSource := filepath.Join(remoteHome, "projects", "repo")
	if err := os.MkdirAll(filepath.Dir(remoteSource), 0o700); err != nil {
		t.Fatal(err)
	}
	git(root, "clone", "-b", "develop", bare, remoteSource)
	// Fake gh reads the same private hosts.yml as the production GitHub CLI.
	gh := "#!/bin/sh\nif [ \"$1 $2\" = 'auth token' ]; then test -f \"$GH_CONFIG_DIR/hosts.yml\" || exit 1; printf fixture-token; exit 0; fi\nexit 0\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "gh"), []byte(gh), 0o700); err != nil {
		t.Fatal(err)
	}
	github := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/graphql" {
			io.WriteString(w, `{"data":{"viewer":{"databaseId":123,"id":"fixture","login":"fixture"}}}`)
			return
		}
		fmt.Fprintf(w, `{"resources":{"core":{"limit":5000,"used":10,"remaining":4990,"reset":%d}}}`, time.Now().Add(time.Hour).Unix())
	}))
	t.Cleanup(github.Close)
	cfg := config.Default()
	cfg.Workspace.Root = filepath.Join(localHome, "workspaces")
	cfg.Workspace.SourceRoot = source
	cfg.Workspace.AutoBranch = true
	cfg.Worker.SSHHosts = []string{"fixture-remote", "local"}
	cfg.Worker.GitHubToken = "fixture-token"
	cfg.Tracker.Kind = config.TrackerGitHub
	cfg.Tracker.APIKey = "fixture-token"
	cfg.Tracker.Endpoint = github.URL + "/graphql"
	cfg.Agent.MaxTurns = 1
	cfg.Agent.MaxSessionDurationMS = 30000
	cfg.Agent.NoProgressTimeoutMS = 30000
	gate := "test -f " + sshQuote(filepath.Join(remoteHome, "before-run")) + " && test -f \"$GH_CONFIG_DIR/hosts.yml\" && printf passed > " + sshQuote(filepath.Join(remoteHome, "gate"))
	cfg.Codex.Command = "env DETENT_SSH_TEST_HELPER=provider DETENT_SSH_TEST_GATE=" + sshQuote(gate) + " " + sshQuote(executable) + " -test.run=^TestSSHHelperProcess$"
	cfg.Gate.Run = gate
	cfg.Budget.Enabled = false
	cfg.Hooks.AfterCreate = "printf created > " + sshQuote(filepath.Join(localHome, "after-create"))
	cfg.Hooks.BeforeRun = "printf before > " + sshQuote(filepath.Join(localHome, "before-run"))
	cfg.Hooks.AfterRun = "printf after > " + sshQuote(filepath.Join(localHome, "after-run"))
	sessions, err := store.Open(ctx, store.Config{Backend: store.BackendSQLite, Path: filepath.Join(root, "sessions.db")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sessions.Close() })
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	built, err := buildRunner(config.Workflow{Config: cfg, Prompt: "Complete the fixture."}, "ssh-fixture", source, globalconfig.Memory{}, gobudget.Budget{}, sessions, logger, serviceapi.Connection{})
	if err != nil {
		t.Fatal(err)
	}
	run := built.(*sshRunner)
	environment := []string{"HOME=" + remoteHome, "TMPDIR=" + root, "PATH=" + filepath.Join(root, "bin") + ":" + os.Getenv("PATH"), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "DETENT_SSH_TEST_HELPER=worker"}
	var lastCommand *exec.Cmd
	var commandMu sync.Mutex
	var commandFactory func(context.Context, string, string) *exec.Cmd
	if useSSH {
		commandFactory = localSSHFixture(t, root, executable, environment)
	} else {
		commandFactory = func(ctx context.Context, host, command string) *exec.Cmd {
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestSSHHelperProcess$")
			cmd.Env = append(append(os.Environ(), environment...), "DETENT_SSH_TEST_HELPER=transport")
			if command == "detent "+SSHProbeArgument {
				cmd.Env = append(cmd.Env, "DETENT_SSH_TEST_HELPER=probe")
			}
			return cmd
		}
	}
	run.command = func(ctx context.Context, host, command string) *exec.Cmd {
		cmd := commandFactory(ctx, host, command)
		if command != "detent "+SSHProbeArgument {
			commandMu.Lock()
			lastCommand = cmd
			commandMu.Unlock()
		}
		return cmd
	}
	if !run.WorkerHostAvailable(ctx, "fixture-remote") {
		t.Fatal("isolated host was not reachable")
	}
	var updatesMu sync.Mutex
	var updates []runnerpkg.UsageUpdate
	request := runnerpkg.RunRequest{ProjectID: "ssh-fixture", Issue: connector.Issue{ID: "first", Identifier: "fixture#1", Title: "SSH fixture", State: "In Progress"}, WorkerHost: "fixture-remote", Mode: runnerpkg.RunModeRoutine, Routine: &runnerpkg.RoutineRequest{Name: "ssh", Prompt: "Create and push feature.txt; run the gate."}, OnUsageUpdate: func(update runnerpkg.UsageUpdate) error {
		updatesMu.Lock()
		updates = append(updates, update)
		updatesMu.Unlock()
		return nil
	}}
	result, err := run.Run(ctx, request)
	if err != nil {
		t.Fatalf("remote lifecycle: %v", err)
	}
	if result.FinalState != runnerpkg.FinalStateCompleted || result.Tokens.TotalTokens != 15 {
		t.Fatalf("remote result = %+v", result)
	}
	for _, marker := range []string{"after-create", "before-run", "after-run", "gate"} {
		if _, err := os.Stat(filepath.Join(remoteHome, marker)); err != nil {
			t.Fatalf("remote %s: %v", marker, err)
		}
		if _, err := os.Stat(filepath.Join(localHome, marker)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("local host affected: %s", marker)
		}
	}
	if got := git(root, "--git-dir", bare, "show", result.WorkspaceBranch+":feature.txt"); got != "remote worker" {
		t.Fatalf("remote push = %q", got)
	}
	updatesMu.Lock()
	for _, update := range updates {
		if update.WorkerProcess.PID != 0 {
			t.Fatal("remote PID reached central process registry")
		}
	}
	updatesMu.Unlock()
	if len(updates) == 0 {
		t.Fatal("no remote usage streamed")
	}
	waitSSHFixtureFile(t, ctx, filepath.Join(remoteHome, "worker-finished"))
	reaped, err := run.ReapWorkspace(ctx, request.Issue)
	if err != nil || reaped.Worktrees != 1 {
		t.Fatalf("remote terminal cleanup: %+v %v", reaped, err)
	}
	if err := os.Remove(filepath.Join(remoteHome, "worker-finished")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(remoteHome, "hold-worker"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	var startOnce sync.Once
	request.Issue.ID, request.Issue.Identifier = "lost", "fixture#2"
	request.OnUsageUpdate = func(update runnerpkg.UsageUpdate) error {
		if update.LastEvent == string(runnerpkg.AgentUpdateTurnStarted) {
			startOnce.Do(func() { close(started) })
		}
		return nil
	}
	supervisor, err := runnerpkg.NewSupervisor(run, runnerpkg.SupervisorConfig{OverloadRetryDelay: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	completed := make(chan runnerpkg.Completion, 1)
	go func() { completed <- supervisor.Run(ctx, request) }()
	select {
	case <-started:
	case completion := <-completed:
		t.Fatalf("worker ended before host-loss fixture: %v", completion.Err)
	case <-ctx.Done():
		t.Fatal("worker never started")
	}
	commandMu.Lock()
	command := lastCommand
	commandMu.Unlock()
	if command == nil || command.Process == nil {
		t.Fatal("host-loss fixture did not capture a running worker command")
	}
	err = command.Process.Kill()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case completion := <-completed:
		if !completion.Retryable || completion.RetryAttempt != request.Attempt && request.Attempt > 0 || !runnerpkg.IsTransientOverload(completion.Err) {
			t.Fatalf("host loss did not use instance retry: %+v", completion)
		}
	case <-ctx.Done():
		t.Fatal("host loss hung the supervisor")
	}
	if run.WorkerHostAvailable(ctx, "fixture-remote") {
		t.Fatal("lost host stayed eligible")
	}
	if !run.WorkerHostAvailable(ctx, "local") {
		t.Fatal("host loss suppressed local execution")
	}
	waitSSHFixtureFile(t, ctx, filepath.Join(remoteHome, "worker-finished"))
	orphans, err := sessions.(store.OrphanSessionStore).ListOrphanedAgentSessions(ctx, "ssh-fixture")
	if err != nil || len(orphans) != 0 {
		t.Fatalf("host loss left open central sessions: %v %v", orphans, err)
	}
	credentials, err := filepath.Glob(filepath.Join(root, "worker-*", "github-cli", "hosts.yml"))
	if err != nil || len(credentials) != 0 {
		t.Fatalf("bootstrap credentials persisted: %v %v", credentials, err)
	}
}

func sshQuote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'" }

func waitSSHFixtureFile(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	timer := time.NewTicker(10 * time.Millisecond)
	defer timer.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			t.Fatalf("remote teardown did not finish: %s", path)
		}
	}
}

func localSSHFixture(t *testing.T, root, executable string, environment []string) func(context.Context, string, string) *exec.Cmd {
	t.Helper()
	sshd, err := exec.LookPath("sshd")
	if err != nil {
		if _, err = os.Stat("/usr/sbin/sshd"); err == nil {
			sshd = "/usr/sbin/sshd"
		} else {
			t.Skip("local SSH integration requires sshd")
		}
	}
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"host-key", "client-key"} {
		cmd := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", filepath.Join(root, name))
		if data, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("ssh-keygen: %v %s", err, data)
		}
	}
	public, err := os.ReadFile(filepath.Join(root, "client-key.pub"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "authorized_keys"), public, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	hostKey, err := os.ReadFile(filepath.Join(root, "host-key.pub"))
	if err != nil {
		t.Fatal(err)
	}
	knownHosts := filepath.Join(root, "known_hosts")
	if err := os.WriteFile(knownHosts, []byte(fmt.Sprintf("[127.0.0.1]:%d %s", port, hostKey)), 0o600); err != nil {
		t.Fatal(err)
	}
	configuration := fmt.Sprintf("Port %d\nListenAddress 127.0.0.1\nHostKey %s\nPidFile %s\nAuthorizedKeysFile %s\nUsePAM no\nPasswordAuthentication no\nKbdInteractiveAuthentication no\nStrictModes no\n", port, filepath.Join(root, "host-key"), filepath.Join(root, "sshd.pid"), filepath.Join(root, "authorized_keys"))
	configPath := filepath.Join(root, "sshd.conf")
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}
	logFile, err := os.Create(filepath.Join(root, "sshd.log"))
	if err != nil {
		t.Fatal(err)
	}
	daemon := exec.Command(sshd, "-D", "-e", "-f", configPath)
	daemon.Stderr = logFile
	if err := daemon.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { daemon.Process.Kill(); daemon.Wait(); logFile.Close() })
	factory := func(ctx context.Context, host, command string) *exec.Cmd {
		vars := append([]string(nil), environment...)
		if command == "detent "+SSHProbeArgument {
			vars = append(vars, "DETENT_SSH_TEST_HELPER=probe")
		}
		remote := "env"
		for _, variable := range vars {
			remote += " " + sshQuote(variable)
		}
		remote += " " + sshQuote(executable) + " -test.run=^TestSSHHelperProcess$"
		return exec.CommandContext(ctx, "ssh", "-T", "-p", strconv.Itoa(port), "-i", filepath.Join(root, "client-key"), "-o", "IdentitiesOnly=yes", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile="+knownHosts, "-o", "ConnectTimeout=2", account.Username+"@127.0.0.1", remote)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	for {
		cmd := factory(ctx, "fixture", "detent "+SSHProbeArgument)
		if output, err := cmd.Output(); err == nil && strings.TrimSpace(string(output)) == strconv.Itoa(runnerpkg.SSHProtocolVersion) {
			return factory
		}
		if ctx.Err() != nil {
			data, _ := os.ReadFile(filepath.Join(root, "sshd.log"))
			t.Fatalf("isolated SSH daemon unreachable: %s", data)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
