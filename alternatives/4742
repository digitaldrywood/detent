package releaseaudit

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"time"
)

type linuxSandbox struct {
	bwrap   string
	prlimit string
	target  string
	scratch string
}

func NewSandbox(target, scratch string) (Sandbox, error) {
	if runtime.GOOS != "linux" || os.Geteuid() == 0 || !filepath.IsAbs(target) || !filepath.IsAbs(scratch) {
		return nil, errors.New("release audit target execution sandbox is unavailable")
	}
	resolvedTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil, err
	}
	resolvedScratch, err := filepath.EvalSymlinks(scratch)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(resolvedTarget, resolvedScratch)
	if err != nil || relative == "." || filepath.IsLocal(relative) {
		return nil, errors.New("release audit scratch must be outside the read-only target")
	}
	bwrap, err := exec.LookPath("bwrap")
	if err != nil {
		return nil, err
	}
	prlimit, err := exec.LookPath("prlimit")
	if err != nil {
		return nil, err
	}
	if err := checkSandboxMemory(); err != nil {
		return nil, err
	}
	return &linuxSandbox{bwrap: bwrap, prlimit: prlimit, target: resolvedTarget, scratch: resolvedScratch}, nil
}

func (s *linuxSandbox) Execute(ctx context.Context, arguments []string, directory string) (string, error) {
	if len(arguments) == 0 || len(arguments) > 64 {
		return "", errors.New("release audit fixture command is invalid")
	}
	relative, err := filepath.Rel(s.scratch, directory)
	if err != nil || !filepath.IsLocal(relative) {
		return "", errors.New("release audit fixture scratch is outside the job")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	argv := []string{"--cpu=30", "--as=" + strconv.FormatInt(1<<30, 10), "--nproc=64", "--fsize=" + strconv.Itoa(8<<20), "--nofile=128", "--", s.bwrap, "--unshare-all", "--die-with-parent", "--new-session", "--clearenv", "--setenv", "PATH", "/usr/bin:/bin", "--setenv", "HOME", "/scratch", "--setenv", "TMPDIR", "/scratch", "--ro-bind", s.target, "/target", "--size", strconv.Itoa(16 << 20), "--tmpfs", "/scratch", "--proc", "/proc", "--remount-ro", "/proc", "--chdir", "/target"}
	for _, root := range []string{"/usr", "/bin", "/lib", "/lib64"} {
		if _, err := os.Stat(root); err == nil {
			argv = append(argv, "--ro-bind", root, root)
		}
	}
	for _, device := range []string{"null", "zero", "random", "urandom"} {
		argv = append(argv, "--ro-bind", "/dev/"+device, "/dev/"+device)
	}
	argv = append(argv, "--remount-ro", "/", "--")
	argv = append(argv, arguments...)
	command := exec.CommandContext(ctx, "prlimit")
	command.Args = append(command.Args, argv...)
	command.Path = s.prlimit
	command.Dir = directory
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=" + directory, "TMPDIR=" + directory}
	output := &boundedOutput{limit: 32768}
	command.Stdout, command.Stderr = output, output
	err = runSandboxCommand(command)
	return output.buffer.String(), err
}

type boundedOutput struct {
	buffer bytes.Buffer
	limit  int
}

func (w *boundedOutput) Write(content []byte) (int, error) {
	if len(content) > w.limit-w.buffer.Len() {
		return 0, errors.New("release audit observation exceeds its byte allowance")
	}
	return w.buffer.Write(content)
}
