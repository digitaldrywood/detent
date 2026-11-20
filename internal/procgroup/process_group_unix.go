//go:build unix

package procgroup

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const workerNiceDelta = 5

func Configure(ctx context.Context, cmd *exec.Cmd) {
	if ctx == nil {
		ctx = context.Background()
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	if err := configureSandboxCapabilities(ctx, cmd); err != nil {
		cmd.Err = errors.Join(cmd.Err, err)
	}
	terminationCtx := context.WithoutCancel(ctx)
	cmd.Cancel = func() error {
		identity, err := Inspect(cmd)
		if err != nil {
			return TerminateTree(cmd, GroupID(cmd))
		}
		_, err = Terminate(terminationCtx, identity, DefaultTerminationGrace)
		return err
	}
}

func GroupID(cmd *exec.Cmd) int {
	if cmd == nil || cmd.Process == nil {
		return 0
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil || pgid <= 0 {
		return cmd.Process.Pid
	}
	return pgid
}

func Deprioritize(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return ErrProcessNotRunning
	}
	current, err := processNice(os.Getpid())
	if err != nil {
		return fmt.Errorf("read Detent process priority: %w", err)
	}
	target := min(19, current+workerNiceDelta)
	if err := unix.Setpriority(unix.PRIO_PROCESS, cmd.Process.Pid, target); errors.Is(err, syscall.ESRCH) {
		return nil
	} else if err != nil {
		return fmt.Errorf("set worker process %d priority: %w", cmd.Process.Pid, err)
	}
	return nil
}

func TerminateTree(cmd *exec.Cmd, processGroupID int) error {
	return terminateTree(cmd, processGroupID, syscall.Kill, inspectProcessGroup)
}

func terminateTree(
	cmd *exec.Cmd,
	processGroupID int,
	signal func(int, syscall.Signal) error,
	inspect func(int) ([]processGroupMember, error),
) error {
	if processGroupID > 0 {
		err := signal(-processGroupID, syscall.SIGKILL)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
		if errors.Is(err, syscall.EPERM) && confirmSignaledGroupExit(processGroupID, signal, inspect) {
			return nil
		}
		return err
	}
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	return cmd.Process.Kill()
}

func confirmSignaledGroupExit(
	processGroupID int,
	signal func(int, syscall.Signal) error,
	inspect func(int) ([]processGroupMember, error),
) bool {
	members, err := inspect(processGroupID)
	if err != nil || !processGroupExited(members) {
		return false
	}
	return len(members) > 0 || errors.Is(signal(-processGroupID, 0), syscall.ESRCH)
}

func Cleanup(processGroupID int) error {
	return cleanup(processGroupID, syscall.Kill, inspectProcessGroup)
}

func cleanup(
	processGroupID int,
	signal func(int, syscall.Signal) error,
	inspect func(int) ([]processGroupMember, error),
) error {
	if processGroupID <= 0 {
		return nil
	}
	err := signal(-processGroupID, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if errors.Is(err, syscall.EPERM) && confirmSignaledGroupExit(processGroupID, signal, inspect) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("signal process group %d with SIGKILL: %w", processGroupID, err)
	}
	if waitForProcessGroupExit(processGroupID, DefaultTerminationGrace) {
		return nil
	}
	members, inspectErr := inspect(processGroupID)
	if inspectErr == nil && processGroupExited(members) {
		return nil
	}
	if !processTargetAlive(0, processGroupID) {
		return nil
	}
	if inspectErr == nil {
		members, inspectErr = inspect(processGroupID)
		if inspectErr == nil && processGroupExited(members) {
			return nil
		}
	}
	return fmt.Errorf(
		"process group %d remained alive after SIGKILL: surviving_processes=%s",
		processGroupID,
		describeProcessGroup(members, inspectErr),
	)
}

func waitForProcessGroupExit(processGroupID int, grace time.Duration) bool {
	if grace <= 0 {
		grace = DefaultTerminationGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !processTargetAlive(0, processGroupID) {
			return true
		}
		select {
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}

type processGroupMember struct {
	pid     string
	ppid    string
	state   string
	command string
}

func processGroupExited(members []processGroupMember) bool {
	if len(members) == 0 {
		return true
	}
	for _, member := range members {
		if !strings.HasPrefix(member.state, "Z") {
			return false
		}
	}
	return true
}

func describeProcessGroup(members []processGroupMember, err error) string {
	if err != nil {
		return "unavailable (" + err.Error() + ")"
	}
	if len(members) == 0 {
		return "none listed"
	}
	descriptions := make([]string, 0, len(members))
	for _, member := range members {
		descriptions = append(descriptions, fmt.Sprintf(
			"pid=%s ppid=%s state=%s command=%s",
			member.pid,
			member.ppid,
			member.state,
			member.command,
		))
	}
	return strings.Join(descriptions, "; ")
}

func Terminate(ctx context.Context, identity Identity, grace time.Duration) (TerminationOutcome, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if identity.PID <= 0 {
		return TerminationOutcomeAlreadyExited, nil
	}
	if identity.GroupID <= 0 || identity.StartedAt.IsZero() {
		return TerminationOutcomeStaleIdentity, nil
	}

	current, err := inspectProcess(identity.PID)
	if err != nil && !errors.Is(err, ErrProcessNotRunning) {
		return "", err
	}
	if err == nil && !sameIdentity(current, identity) {
		return TerminationOutcomeStaleIdentity, nil
	}
	groupID := identity.GroupID
	if !processTargetAlive(identity.PID, groupID) {
		return TerminationOutcomeAlreadyExited, nil
	}
	if err := signalProcessTarget(identity.PID, groupID, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return TerminationOutcomeAlreadyExited, nil
		}
		return "", err
	}
	if waitForProcessTargetExit(ctx, identity.PID, groupID, grace) ||
		confirmProcessTargetExit(identity.PID, groupID, signalProcessTarget, inspectProcessGroup) {
		return TerminationOutcomeTerminated, nil
	}
	if err := signalProcessTarget(identity.PID, groupID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		if !errors.Is(err, syscall.EPERM) ||
			!confirmProcessTargetExit(identity.PID, groupID, signalProcessTarget, inspectProcessGroup) {
			return "", err
		}
		return TerminationOutcomeKilled, nil
	}
	if !waitForProcessTargetExit(context.Background(), identity.PID, groupID, grace) &&
		!confirmProcessTargetExit(identity.PID, groupID, signalProcessTarget, inspectProcessGroup) {
		return "", fmt.Errorf("process group %d remained alive after SIGKILL", groupID)
	}
	return TerminationOutcomeKilled, nil
}

type observedProcess struct {
	identity Identity
	rssBytes int64
	zombie   bool
}

func psCommand(args ...string) (*exec.Cmd, error) {
	path, err := exec.LookPath("ps")
	if err != nil {
		return nil, fmt.Errorf("locate ps: %w", err)
	}
	return &exec.Cmd{
		Path: path,
		Args: append([]string{"ps"}, args...),
		Env:  append(os.Environ(), "LC_ALL=C"),
	}, nil
}

func observeProcesses(identities []Identity) ([]Observation, error) {
	cmd, err := psCommand("-axo", "pid=,pgid=,rss=,stat=,lstart=")
	if err != nil {
		return nil, err
	}
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("inspect worker processes: %w", err)
	}

	byPID := make(map[int]observedProcess)
	byGroup := make(map[int][]observedProcess)
	for line := range strings.Lines(string(output)) {
		fields := strings.Fields(line)
		if len(fields) != 9 {
			continue
		}
		pid, pidErr := strconv.Atoi(fields[0])
		groupID, groupErr := strconv.Atoi(fields[1])
		rssKB, rssErr := strconv.ParseInt(fields[2], 10, 64)
		startedAt, startedErr := time.ParseInLocation("Mon Jan 2 15:04:05 2006", strings.Join(fields[4:], " "), time.Local)
		if pidErr != nil || groupErr != nil || rssErr != nil || startedErr != nil {
			continue
		}
		process := observedProcess{
			identity: Identity{PID: pid, GroupID: groupID, StartedAt: startedAt.UTC()},
			rssBytes: max(rssKB, 0) * 1024,
			zombie:   strings.HasPrefix(fields[3], "Z"),
		}
		byPID[pid] = process
		byGroup[groupID] = append(byGroup[groupID], process)
	}

	observations := make([]Observation, 0, len(identities))
	for _, identity := range identities {
		observation := Observation{Identity: identity, GroupObserved: true}
		if identity.PID <= 0 || identity.StartedAt.IsZero() {
			observations = append(observations, observation)
			continue
		}
		leader, leaderFound := byPID[identity.PID]
		if leaderFound && !sameIdentity(leader.identity, identity) {
			observation.Stale = true
			observations = append(observations, observation)
			continue
		}
		groupID := identity.GroupID
		if groupID <= 0 {
			groupID = identity.PID
		}
		for _, process := range byGroup[groupID] {
			if process.zombie {
				continue
			}
			observation.ProcessCount++
			observation.RSSBytes += process.rssBytes
		}
		observation.Alive = observation.ProcessCount > 0
		observations = append(observations, observation)
	}
	return observations, nil
}

func signalProcessTarget(pid int, groupID int, signal syscall.Signal) error {
	if groupID > 0 {
		return syscall.Kill(-groupID, signal)
	}
	return syscall.Kill(pid, signal)
}

func processTargetAlive(pid int, groupID int) bool {
	err := signalProcessTarget(pid, groupID, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func confirmProcessTargetExit(
	pid int,
	groupID int,
	signal func(int, int, syscall.Signal) error,
	inspect func(int) ([]processGroupMember, error),
) bool {
	if groupID > 0 {
		members, err := inspect(groupID)
		if err == nil && len(members) > 0 {
			return processGroupExited(members)
		}
	}
	return errors.Is(signal(pid, groupID, 0), syscall.ESRCH)
}

func waitForProcessTargetExit(ctx context.Context, pid int, groupID int, grace time.Duration) bool {
	if grace <= 0 {
		grace = DefaultTerminationGrace
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		if !processTargetAlive(pid, groupID) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return false
		case <-ticker.C:
		}
	}
}

func startAcknowledged(cmd *exec.Cmd, acknowledge func(Identity) error) error {
	if cmd.Err != nil {
		return cmd.Err
	}
	fd := 3 + len(cmd.ExtraFiles)
	if fd > 9 {
		return errors.New("no descriptor available for worker acknowledgement")
	}
	input, release, err := os.Pipe()
	if err != nil {
		return fmt.Errorf("open worker acknowledgement pipe: %w", err)
	}
	defer release.Close()
	cmd.ExtraFiles = append(cmd.ExtraFiles, input)
	script := fmt.Sprintf(`IFS= read -r permission <&%d && [ "$permission" = go ] || exit 1; exec %d<&-; exec "$@"`, fd, fd)
	args := []string{"sh", "-c", script, "sh", cmd.Path}
	if len(cmd.Args) > 0 {
		args = append(args, cmd.Args[1:]...)
	}
	cmd.Args = args
	cmd.Path = "/bin/sh"
	if err := Start(cmd); err != nil {
		return errors.Join(err, input.Close())
	}
	group := GroupID(cmd)
	if err := input.Close(); err != nil {
		return errors.Join(err, release.Close(), TerminateTree(cmd, group), Wait(cmd))
	}
	identity, err := Inspect(cmd)
	if err == nil {
		err = acknowledge(identity)
	}
	if err != nil {
		return errors.Join(fmt.Errorf("acknowledge worker launch: %w", err), release.Close(), Wait(cmd))
	}
	if _, err := release.WriteString("go\n"); err != nil {
		return errors.Join(err, release.Close(), TerminateTree(cmd, group), Wait(cmd))
	}
	return nil
}
