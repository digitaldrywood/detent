import json
from pathlib import Path
import sys

source, output = map(Path, sys.argv[1:])
output.mkdir(exist_ok=True)
replacements = {}


def overlay(relative, edits):
    original = source / relative
    content = original.read_text()
    for old, new in edits:
        if content.count(old) != 1:
            raise ValueError(f"Diagnostic overlay anchor changed in {relative}")
        content = content.replace(old, new)
    target = output / original.name
    target.write_text(content)
    replacements[str(original)] = str(target)


overlay("internal/hubclient/api/client.go", [
    ('\n\t"net/url"\n', '\n\t"net/url"\n\t"os"\n'),
    ('\tresponse, err := httpClient.Do(request)',
     '''	if mutation, ok := input.(runnersession.Mutation); ok && method == http.MethodPost && strings.HasSuffix(requestPath, "/runner-sessions") {
		fmt.Fprintf(os.Stderr, "SMOKE_SESSION_POST %s %d\\n", mutation.LeaseID, mutation.FencingToken)
	}
	response, err := httpClient.Do(request)'''),
    ('\tif result, ok := output.(*responseWithHeaders); ok {',
     '''	if receipt, ok := input.(tracker.NativePhaseReceipt); ok && method == http.MethodPost && strings.HasSuffix(requestPath, "/phases") {
		fmt.Fprintf(os.Stderr, "SMOKE_PHASE_ACK %s %s %s\\n", receipt.AttemptID, receipt.Phase, time.Now().UTC().Format(time.RFC3339Nano))
	}
	if result, ok := output.(*responseWithHeaders); ok {'''),
])

overlay("internal/cli/update_runtime.go", [
    ('if !detentupdate.IsDevelopmentVersion(version) {\n\t\tservice, ok',
     '''if sourceRoot := os.Getenv("DETENT_LOCAL_SOURCE_WORKERS"); sourceRoot != "" && detentupdate.IsDevelopmentVersion(version) {
		schedulerConfig.WorkerArtifacts, err = detentupdate.NewSourceWorkerArtifacts(sourceRoot, executable, version, cfg.Build.Commit)
		if err != nil {
			return nil, err
		}
		schedulerConfig.WorkerPreflight = workerProtocolPreflight
	} else if !detentupdate.IsDevelopmentVersion(version) {
		service, ok'''),
])
overlay("internal/update/worker_artifacts.go", [
    ("type WorkerArtifacts struct {", "type WorkerArtifacts struct {\n\tsourceWorker bool"),
    ("if !a.prepared.VerifiedRelease {", "if !a.prepared.VerifiedRelease && !a.sourceWorker {"),
    ("\treleases, err := a.service.cfg.Client.ListReleases(ctx)",
     '''	if a.sourceWorker {
		return a.AcquireLaunch(ctx, preflight, capture)
	}
	releases, err := a.service.cfg.Client.ListReleases(ctx)'''),
    ("func (a *WorkerArtifact) Release() error {",
     '''func NewSourceWorkerArtifacts(root, binary, version, commit string) (*WorkerArtifacts, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(binary) || !IsDevelopmentVersion(version) {
		return nil, errors.New("source worker requires a development build and absolute paths")
	}
	data, err := os.ReadFile(binary)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	hash := hex.EncodeToString(digest[:])
	directory := filepath.Join(root, "worker-"+hash)
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	executable := filepath.Join(directory, projectName)
	if err := os.Link(binary, executable); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	return &WorkerArtifacts{
		sourceWorker: true, root: root, live: make(map[string]int), latest: directory,
		prepared: Status{CurrentVersion: version, LatestVersion: version, LatestCommit: commit, Binary: executable, BinarySHA256: hash},
	}, nil
}

func (a *WorkerArtifact) Release() error {'''),
])
overlay("internal/cli/local_worker.go", [
    ("cmd.Stdout, cmd.Stderr = childOutput, io.Discard",
     '''cmd.Stdout, cmd.Stderr = childOutput, io.Discard
	if os.Getenv("DETENT_LOCAL_SOURCE_WORKERS") != "" {
		cmd.Stderr = os.Stderr
	}'''),
])
worker_sessions = "\tvar sessions runnerpkg.SessionStore = runnerpkg.SSHSessionStore{Peer: peer}"
if "\tsessions := boot.sessionStore(peer)" in (source / "internal/cli/ssh_runner.go").read_text():
    worker_sessions = "\tsessions := boot.sessionStore(peer)"

overlay("internal/cli/ssh_runner.go", [
    (worker_sessions,
     '''	if marker := os.Getenv("DETENT_LOCAL_STARTUP_REFUSAL"); boot.Local && operation == "run" && marker != "" {
		if raw, err := os.ReadFile(marker); err == nil {
			port, err := strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil || port < 1024 || port > 65535 {
				return nil, errors.New("invalid local startup fault endpoint")
			}
			if err := os.Rename(marker, marker+".used"); err != nil {
				return nil, err
			}
			logger.Info("injected local worker startup refusal", "pid", os.Getpid(), "observed_at", time.Now().UTC())
			conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
			if err != nil {
				return nil, fmt.Errorf("%w: local startup identity endpoint: %w", runnerpkg.ErrWorkspacePreparation, err)
			}
			_ = conn.Close()
			return nil, errors.New("local startup fault endpoint unexpectedly accepted a connection")
		}
	}
''' + worker_sessions),
])
overlay("internal/workspace/merge.go", [
    ("\tcmd := commandshell.Command(ctx, command, l.hooks.Shell)\n\tcmd.Dir = commandInfo.Path",
     '''	actual := command
	armed := os.Getenv("DETENT_LOCAL_GATE_ARMED")
	driver := os.Getenv("DETENT_LOCAL_GATE_DRIVER")
	python := os.Getenv("DETENT_LOCAL_GATE_PYTHON")
	injected := native && command == "make check" && armed != "" && driver != "" && python != "" && os.Rename(armed, armed+".used") == nil
	if injected {
		actual = `exec "$DETENT_LOCAL_GATE_PYTHON" "$DETENT_LOCAL_GATE_DRIVER" "$DETENT_LOCAL_GATE_ARMED"`
	}
	cmd := commandshell.Command(ctx, actual, l.hooks.Shell)
	cmd.Dir = commandInfo.Path'''),
    ("\tcmd.Env = hookEnv(commandInfo, issue, l.hooks.StripGitHubTokens)",
     '''	cmd.Env = hookEnv(commandInfo, issue, l.hooks.StripGitHubTokens)
	if injected {
		procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: map[string]string{
			"DETENT_LOCAL_GATE_PYTHON": python, "DETENT_LOCAL_GATE_DRIVER": driver,
			"DETENT_LOCAL_GATE_ARMED": armed + ".used", "DETENT_LOCAL_GATE_WORKTREE": info.Path,
			"DETENT_LOCAL_GATE_HEAD": issue.PullRequestHeadSHA,
		}})
	}'''),
])
(output / "overlay.json").write_text(json.dumps({"Replace": replacements}))
