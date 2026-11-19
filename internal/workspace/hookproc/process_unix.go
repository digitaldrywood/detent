//go:build unix

package hookproc

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspace/internal/workpath"

	"github.com/digitaldrywood/detent/internal/procstart"
	"github.com/digitaldrywood/detent/internal/runtimeoutput"
)

const minimumWorkspaceProcessScanTimeout = 30 * time.Second

func ReapProcesses(ctx context.Context, path string, grace time.Duration) (int, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return 0, errors.New("workspace path is required")
	}
	if !filepath.IsAbs(path) {
		return 0, fmt.Errorf("workspace path must be absolute: %q", path)
	}
	path = filepath.Clean(path)
	if filepath.Dir(path) == path {
		return 0, fmt.Errorf("workspace path must not be a filesystem root: %q", path)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if grace <= 0 {
		grace = defaultProcessTerminationGrace
	}
	// The process inventory has independent scratch-environment and cwd stages,
	// and reaping may inventory again after each signal. Bound a stalled stage,
	// but renew the budget after each completed stage so healthy progress is not
	// constrained by one aggregate wall-clock deadline.
	ctx = withWorkspaceProcessScanBudget(ctx, max(2*grace, minimumWorkspaceProcessScanTimeout), context.WithTimeout)
	scan := ReapProcessScanner
	if source, ok := ctx.Value(processSourceKey{}).(ProcessSource); ok && source != nil && ctx.Value(hookOwnerKey{}) == nil {
		scan = Scanner(source)
	}
	return reapVerifiedProcesses(ctx, path, grace, scan, procstart.Identity, processEnvironment, nil)
}

func reapVerifiedProcesses(ctx context.Context, path string, grace time.Duration, scan Scanner, identity func(int) (string, error), environment func(int) ([]string, error), signal workspaceProcessSignaler) (int, error) {
	verified, guardedSignal := verifiedProcessTargets(ctx, path, scan, identity, environment, signal)
	return reapProcesses(ctx, path, grace, verified, guardedSignal)
}

type hookSignals interface {
	prepare(int) (func(), error)
	signal(int, syscall.Signal) error
}

func verifiedProcessTargets(ctx context.Context, path string, scan Scanner, identity func(int) (string, error), environment func(int) ([]string, error), signal workspaceProcessSignaler) (Scanner, workspaceProcessSignaler) {
	starts := map[int]string{}
	owner, _ := ctx.Value(hookOwnerKey{}).(string)
	confirmed := map[int]string{}
	lifetime, _ := ctx.Value(lifetimeKey{}).(*Lifetime)
	var bindings hookSignals
	if owner != "" && signal == nil {
		bindings = platformHookSignals(lifetime)
	}
	if lifetime != nil {
		confirmed = lifetime.starts
	}
	for pid, start := range confirmed {
		starts[pid] = start
	}
	sameProcess := func(pid int) (bool, error) {
		current, err := identity(pid)
		if errors.Is(err, procstart.ErrNotRunning) {
			return false, nil
		}
		return err == nil && starts[pid] != "" && current == starts[pid], err
	}
	stillOwned := func(pid int) (bool, error) {
		if starts[pid] == "" {
			return false, nil
		}
		env, err := environment(pid)
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH) {
			return false, nil
		}
		if err != nil {
			return false, scratchProcessInspectionError(ctx, pid, "workspace ownership", err, func(context.Context) (bool, error) { return sameProcess(pid) })
		}
		if !hookSignalAllowed(owner, path, env) {
			lifetime, ok := ctx.Value(lifetimeKey{}).(*Lifetime)
			if !ok || !hookDescriptorAllowed(owner, path, env) {
				return false, nil
			}
			held, err := lifetime.holdsWriter(ctx, pid)
			if err != nil || !held {
				return false, err
			}
		}
		return sameProcess(pid)
	}
	observeWriters := lifetime != nil
	verified := func(ctx context.Context, path string) ([]int, error) {
		var writerErr error
		if observeWriters {
			ctx, writerErr = lifetime.recordWriters(ctx)
			observeWriters = false
		}
		candidates, err := ScanOwned(ctx, path, scan)
		err = errors.Join(err, writerErr)
		var fresh bool
		for _, pid := range candidates {
			if _, seen := starts[pid]; seen {
				continue
			}
			start, inspectErr := identity(pid)
			if errors.Is(inspectErr, procstart.ErrNotRunning) {
				continue
			}
			if inspectErr != nil {
				err = errors.Join(err, inspectErr)
				continue
			}
			starts[pid] = start
			fresh = true
		}
		if fresh && owner == "" {
			verified, verifyErr := ScanOwned(ctx, path, scan)
			candidates = verified
			err = errors.Join(err, verifyErr)
		}
		if owner != "" && err == nil {
			seen := make(map[int]bool, len(candidates))
			for _, pid := range candidates {
				seen[pid] = true
			}
			for pid := range confirmed {
				if !seen[pid] {
					candidates = append(candidates, pid)
				}
			}
		}
		var owned []int
		for _, pid := range candidates {
			check := stillOwned
			if confirmed[pid] != "" {
				check = sameProcess
			}
			var accept func()
			var tokenErr error
			if bindings != nil {
				accept, tokenErr = bindings.prepare(pid)
			}
			ok, inspectErr := check(pid)
			err = errors.Join(err, inspectErr)
			if !ok {
				continue
			}
			if tokenErr != nil {
				if lifetime != nil {
					lifetime.recordWitness(pid, starts[pid])
				}
				err = errors.Join(err, tokenErr)
				continue
			}
			if accept != nil {
				accept()
			}
			confirmed[pid] = starts[pid]
			owned = append(owned, pid)
		}
		return owned, err
	}
	guardedSignal := func(pid int, sig syscall.Signal) error {
		send := signal
		if bindings != nil {
			send = bindings.signal
		}
		if send == nil {
			process, err := os.FindProcess(pid)
			if err != nil {
				return err
			}
			defer process.Release()
			send = func(_ int, signal syscall.Signal) error {
				err := process.Signal(signal)
				if errors.Is(err, os.ErrProcessDone) {
					return syscall.ESRCH
				}
				return err
			}
		}
		check := stillOwned
		if owner != "" && confirmed[pid] != "" {
			check = sameProcess
		}
		ok, err := check(pid)
		if err != nil {
			return err
		}
		if !ok {
			return syscall.ESRCH
		}
		return send(pid, sig)
	}
	return verified, guardedSignal
}

var ReapProcessScanner Scanner = ProcessIDs

type workspaceProcessScanContextFactory func(context.Context, time.Duration) (context.Context, context.CancelFunc)

type workspaceProcessScanBudget struct {
	timeout     time.Duration
	withTimeout workspaceProcessScanContextFactory
}

type workspaceProcessScanBudgetKey struct{}

func withWorkspaceProcessScanBudget(
	ctx context.Context,
	timeout time.Duration,
	withTimeout workspaceProcessScanContextFactory,
) context.Context {
	return context.WithValue(ctx, workspaceProcessScanBudgetKey{}, workspaceProcessScanBudget{
		timeout:     timeout,
		withTimeout: withTimeout,
	})
}

func runWorkspaceProcessScan(ctx context.Context, path string, scan Scanner) ([]int, error) {
	budget, ok := ctx.Value(workspaceProcessScanBudgetKey{}).(workspaceProcessScanBudget)
	if !ok || budget.timeout <= 0 || budget.withTimeout == nil {
		// Reconciliation also inventories processes without going through
		// ReapProcesses. Give it the same stage budget as reaping.
		budget = workspaceProcessScanBudget{
			timeout:     minimumWorkspaceProcessScanTimeout,
			withTimeout: context.WithTimeout,
		}
	}
	scanCtx, cancel := budget.withTimeout(ctx, budget.timeout)
	defer cancel()
	return scan(scanCtx, path)
}

type workspaceProcessSignaler func(int, syscall.Signal) error
type workspaceProcessWaiter func(context.Context, string, time.Duration, Scanner) ([]int, error)

func reapProcesses(
	ctx context.Context,
	path string,
	grace time.Duration,
	scan Scanner,
	signal workspaceProcessSignaler,
) (int, error) {
	return reapProcessesWithWait(ctx, path, grace, scan, signal, waitForWorkspaceProcesses)
}

func reapProcessesWithWait(
	ctx context.Context,
	path string,
	grace time.Duration,
	scan Scanner,
	signal workspaceProcessSignaler,
	wait workspaceProcessWaiter,
) (int, error) {
	pids, err := ScanOwned(ctx, path, scan)
	if err != nil {
		scanErr := fmt.Errorf("scan workspace processes: %w", err)
		if len(pids) == 0 {
			return 0, scanErr
		}
		reaped := make(map[int]struct{}, len(pids))
		signalErr := signalWorkspaceProcesses(pids, syscall.SIGTERM, signal, reaped)
		return len(reaped), errors.Join(scanErr, signalErr)
	}
	if len(pids) == 0 {
		return 0, nil
	}

	reaped := make(map[int]struct{}, len(pids))
	signalErr := signalWorkspaceProcesses(pids, syscall.SIGTERM, signal, reaped)
	killed := map[int]struct{}{}
	for {
		survivors, err := wait(ctx, path, grace, scan)
		if err != nil || len(survivors) == 0 || signalErr != nil {
			return len(reaped), errors.Join(signalErr, err)
		}
		var newcomers, escalate []int
		for _, pid := range survivors {
			_, termSent := reaped[pid]
			_, killSent := killed[pid]
			if killSent || !termSent && len(killed) > 0 {
				return len(reaped), fmt.Errorf("workspace processes remained after SIGKILL: pid=%d", pid)
			}
			if termSent {
				escalate = append(escalate, pid)
			} else {
				newcomers = append(newcomers, pid)
			}
		}
		if len(newcomers) > 0 {
			if _, bounded := ctx.Deadline(); !bounded {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, minimumWorkspaceProcessScanTimeout)
				defer cancel()
			}
			signalErr = signalWorkspaceProcesses(newcomers, syscall.SIGTERM, signal, reaped)
		}
		signalErr = errors.Join(signalErr, signalWorkspaceProcesses(escalate, syscall.SIGKILL, signal, killed))
	}
}

func signalWorkspaceProcesses(
	pids []int,
	sig syscall.Signal,
	signal workspaceProcessSignaler,
	reaped map[int]struct{},
) error {
	var result error
	for _, pid := range pids {
		if err := signal(pid, sig); errors.Is(err, syscall.ESRCH) {
			continue
		} else if err != nil {
			result = errors.Join(result, fmt.Errorf("signal workspace process %d with %s: %w", pid, sig, err))
			continue
		}
		reaped[pid] = struct{}{}
	}
	return result
}

func waitForWorkspaceProcesses(
	ctx context.Context,
	path string,
	grace time.Duration,
	scan Scanner,
) ([]int, error) {
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		pids, err := ScanOwned(ctx, path, scan)
		if err != nil {
			return nil, fmt.Errorf("verify workspace processes: %w", err)
		}
		if len(pids) == 0 {
			return nil, nil
		}
		select {
		case <-ctx.Done():
			return pids, ctx.Err()
		case <-timer.C:
			return pids, nil
		case <-ticker.C:
		}
	}
}

func ProcessIDs(ctx context.Context, path string) ([]int, error) {
	if lifetime, ok := ctx.Value(lifetimeKey{}).(*Lifetime); ok {
		marked, envErr := scratchEnvironmentProcessIDs(ctx, path)
		holders, fdErr := lifetime.candidates(ctx)
		return append(marked, holders...), errors.Join(envErr, fdErr)
	}
	if source, ok := ctx.Value(processSourceKey{}).(ProcessSource); ok && source != nil {
		return source(ctx, path)
	}

	canonical, err := workpath.CanonicalExistingPath(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if filepath.Dir(canonical) == canonical {
		return nil, errors.New("workspace path must not resolve to a filesystem root")
	}
	path = canonical
	cwdScan := func(ctx context.Context, path string) ([]int, error) {
		if runtime.GOOS == "linux" {
			return linuxWorkspaceProcessIDs(path)
		}
		return lsofWorkspaceProcessIDs(ctx, path)
	}
	return workspaceProcessIDsWithScanners(ctx, path, scratchEnvironmentProcessIDs, cwdScan)
}

func workspaceProcessIDsWithScanners(
	ctx context.Context,
	path string,
	scratchScan Scanner,
	cwdScan Scanner,
) ([]int, error) {
	owned, scratchErr := runWorkspaceProcessScan(ctx, path, scratchScan)
	if err := ctx.Err(); err != nil {
		return owned, errors.Join(scratchErr, err)
	}
	cwd, cwdErr := runWorkspaceProcessScan(ctx, path, cwdScan)
	return append(owned, cwd...), errors.Join(scratchErr, cwdErr)
}

func linuxWorkspaceProcessIDs(path string) ([]int, error) {
	root := filepath.Clean(path)
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}

	seen := map[int]struct{}{}
	pids := []int{}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		cwd, err := os.Readlink(filepath.Join("/proc", entry.Name(), "cwd"))
		if err != nil {
			continue
		}
		cwd = strings.TrimSuffix(cwd, " (deleted)")
		if !pathInside(root, cwd) {
			continue
		}
		if _, ok := seen[pid]; ok {
			continue
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	return pids, nil
}

func lsofOutput(ctx context.Context, args ...string) ([]byte, error) {
	return workspaceScanOutput(ctx, exec.CommandContext(ctx, "lsof", args...))
}

func lsofNoMatches(err error) bool {
	var exited *exec.ExitError
	return errors.As(err, &exited) && exited.ExitCode() == 1
}

func lsofWorkspaceProcessIDs(ctx context.Context, path string) ([]int, error) {
	// List every process's working directory and match by prefix in Go.
	// lsof +D walks the whole tree, which on a multi-gigabyte worktree
	// outlives every scan deadline; -d cwd alone is one row per process.
	// -O disables lsof's per-operation fork timeout machinery: on macOS its
	// global filesystem lookup overhead can exhaust the deadline before output.
	output, err := lsofOutput(ctx, "-O", "-w", "-d", "cwd", "-F", "pn")
	if err != nil && len(output) == 0 {
		if lsofNoMatches(err) {
			return nil, nil
		}
		return nil, err
	}
	return parseLsofCwdProcessIDs(string(output), path), nil
}

// parseLsofCwdProcessIDs reads lsof -F pn records (a p<pid> line followed by
// the process's n<cwd> line) and keeps the processes whose cwd is inside root.
func parseLsofCwdProcessIDs(output string, root string) []int {
	root = filepath.Clean(root)
	seen := map[int]struct{}{}
	pids := []int{}
	pid := 0
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}
		switch line[0] {
		case 'p':
			parsed, err := strconv.Atoi(strings.TrimSpace(line[1:]))
			if err != nil {
				parsed = 0
			}
			pid = parsed
		case 'n':
			if pid <= 0 {
				continue
			}
			cwd := strings.TrimSuffix(strings.TrimSpace(line[1:]), " (deleted)")
			if !pathInside(root, cwd) {
				continue
			}
			if _, ok := seen[pid]; ok {
				continue
			}
			seen[pid] = struct{}{}
			pids = append(pids, pid)
		}
	}
	return pids
}

type workspaceScanStderr struct {
	*runtimeoutput.Buffer
}

func (w workspaceScanStderr) Write(data []byte) (int, error) {
	w.Append(string(data))
	return len(data), nil
}

func workspaceScanOutput(ctx context.Context, cmd *exec.Cmd) ([]byte, error) {
	stderr := workspaceScanStderr{runtimeoutput.NewBuffer(runtimeoutput.Policy{MaxBytes: 64 << 10})}
	captureStderr := cmd.Stderr == nil
	if captureStderr {
		cmd.Stderr = stderr
	}
	var output bytes.Buffer
	cmd.Stdout = &output
	started := time.Now()
	remaining := time.Duration(0)
	hasDeadline := false
	if deadline, ok := ctx.Deadline(); ok {
		remaining = time.Until(deadline)
		hasDeadline = true
	}
	contextAtStart := fmt.Sprint(ctx.Err())
	wait, startErr := commandshell.StartCommand(ctx, cmd)
	startElapsed := time.Since(started)
	if startErr != nil {
		return nil, fmt.Errorf("workspace scan command stage=start start_elapsed=%s deadline_set=%t deadline_remaining=%s context_at_start=%v context_error=%v: %w",
			startElapsed, hasDeadline, remaining, contextAtStart, fmt.Sprint(ctx.Err()), startErr)
	}
	waiting := time.Now()
	// Keep the existing context deadline authoritative even if Wait is stuck
	// in an uninterruptible kernel operation or draining inherited pipes.
	// The waiter still owns and eventually reaps the command. Only it reads
	// the buffers, so cancellation can return without racing output writers.
	type result struct {
		output []byte
		err    error
	}
	done := make(chan result, 1)
	go func() {
		err := wait()
		var exitErr *exec.ExitError
		if captureStderr && errors.As(err, &exitErr) {
			exitErr.Stderr = []byte(stderr.String())
		}
		done <- result{output: output.Bytes(), err: err}
	}()
	var completed result
	outputBytes := "unknown"
	select {
	case completed = <-done:
		outputBytes = strconv.Itoa(len(completed.output))
	case <-ctx.Done():
		// Wait still owns the buffer; no output count is available yet.
		// An empty result here does not mean the child emitted no output.
		completed.err = ctx.Err()
	}
	err := completed.err
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	if err != nil {
		return completed.output, fmt.Errorf("workspace scan command stage=wait pid=%d start_elapsed=%s wait_elapsed=%s deadline_set=%t deadline_remaining=%s context_at_start=%v context_error=%v output_bytes=%s: %w",
			cmd.Process.Pid, startElapsed, time.Since(waiting), hasDeadline, remaining, contextAtStart, fmt.Sprint(ctx.Err()), outputBytes, err)
	}
	return completed.output, nil
}

func pathInside(root string, path string) bool {
	path = filepath.Clean(path)
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel))
}
