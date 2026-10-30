package gotestreceipt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/digitaldrywood/detent/internal/procgroup"
)

var runtimeEnvironment = []string{"GODEBUG", "GOGC", "GOMEMLIMIT", "GOMAXPROCS", "GOTRACEBACK", "GORACE", "GOFIPS140", "CGO_ENABLED"}

var cacheableTestFlags = map[string]bool{
	"paniconexit0": true,
	"timeout":      true,
	"short":        true,
	"run":          true,
	"skip":         true,
	"parallel":     true,
	"failfast":     true,
	"cpu":          true,
	"v":            true,
	"count":        true,
	"shuffle":      true,
	"fullpath":     true,
}

func IsWrapperInvocation(arg0 string) bool {
	return strings.TrimSuffix(filepath.Base(arg0), ".exe") == WrapperName
}

func RunWrapper(args []string, getenv func(string) string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintf(stderr, "%s: missing program\n", WrapperName)
		return 2
	}
	socket, root := getenv(SocketEnvironment), getenv(RootEnvironment)
	if socket != "" && root != "" && isTestBinary(args[0]) {
		pwd, err := os.Getwd()
		if err == nil {
			return requestExecution(socket, executionRequest{Args: args, Directory: pwd, Root: root, Environment: os.Environ()}, stdout, stderr)
		}
	}
	return run(args, true, stdin, stdout, stderr)
}

func executeWrapper(ctx context.Context, request executionRequest, client executionStore, stdout, stderr io.Writer) int {
	args, pwd, root := request.Args, request.Directory, request.Root
	getenv := func(name string) string { value, _ := request.lookup(name); return value }
	run := func(args []string, _ bool, stdin io.Reader, stdout, stderr io.Writer) int {
		return runEnvironment(ctx, args, request.Directory, request.Environment, stdin, stdout, stderr)
	}
	var stdin io.Reader
	if !isTestBinary(args[0]) {
		return run(args, false, stdin, stdout, stderr)
	}
	if root == "" || !Cacheable(args[1:]) {
		return run(args, true, stdin, stdout, stderr)
	}
	key, err := Key(args[0], args[1:], getenv)
	if err != nil {
		return run(args, true, stdin, stdout, stderr)
	}
	env := wrapperEnvironment(root, args[0], getenv)
	env.LookupEnv = request.lookup
	env.DigestKey = client.session.execution.DigestKey
	candidates, err := client.Lookup(ctx, key)
	if err != nil {
		fmt.Fprintf(stderr, "%s: receipt lookup unavailable: %v\n", WrapperName, err)
	}
	for _, candidate := range candidates {
		digest, err := Digest(candidate.Inputs, env)
		if err == nil && digest == candidate.Digest {
			if err := client.Hit(ctx, key); err != nil {
				fmt.Fprintf(stderr, "%s: report receipt reuse: %v\n", WrapperName, err)
			}
			fmt.Fprintln(stdout, "PASS")
			return 0
		}
	}
	logFile, err := os.CreateTemp("", "detent-test-log-*")
	if err != nil {
		return run(args, true, stdin, stdout, stderr)
	}
	logPath := logFile.Name()
	var code int
	if err := logFile.Close(); err != nil {
		code = run(args, true, stdin, stdout, stderr)
	} else {
		start := time.Now()
		code = run(append(append([]string{args[0]}, args[1:]...), "-test.testlogfile="+logPath), true, stdin, stdout, stderr)
		if code == 0 {
			if err := record(ctx, client, key, pwd, logPath, env, start); err != nil {
				fmt.Fprintf(stderr, "%s: receipt not recorded: %v\n", WrapperName, err)
				if err := client.Refuse(ctx, Refusal{Package: truncate(packageName(pwd, env)), Reason: truncate(err.Error())}); err != nil {
					fmt.Fprintf(stderr, "%s: report refusal: %v\n", WrapperName, err)
				}
			}
		}
	}
	if err := os.Remove(logPath); err != nil {
		fmt.Fprintf(stderr, "%s: remove test log: %v\n", WrapperName, err)
	}
	return code
}

func record(ctx context.Context, client executionStore, key, pwd, logPath string, env Environment, start time.Time) error {
	inputs, err := readTestLog(logPath, pwd, env, start)
	if err != nil {
		return err
	}
	if err := ModifiedSince(inputs, env, start); err != nil {
		return err
	}
	digest, err := Digest(inputs, env)
	if err != nil {
		return err
	}
	return client.Record(ctx, Receipt{Key: key, Package: packageName(pwd, env), Inputs: inputs, Digest: digest})
}

func packageName(pwd string, env Environment) string {
	if scope, path, _ := env.classify(pwd); scope == scopeRoot {
		return path
	}
	return "."
}

func Cacheable(args []string) bool {
	for _, arg := range args {
		name, value, _ := strings.Cut(strings.TrimPrefix(arg, "-test."), "=")
		if !strings.HasPrefix(arg, "-test.") || !cacheableTestFlags[name] {
			return false
		}
		switch name {
		case "v":
			if value != "false" {
				return false
			}
		case "count":
			return false
		case "shuffle":
			if value != "" && value != "off" {
				return false
			}
		}
	}
	return true
}

func readTestLog(path, pwd string, env Environment, start time.Time) (inputs []string, err error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open test log: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	return ParseTestLog(file, pwd, env, start)
}

func Key(binary string, args []string, getenv func(string) string) (string, error) {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\n%s/%s\n", protocolFingerprint, runtime.GOOS, runtime.GOARCH)
	if err := copyFile(hash, binary); err != nil {
		return "", fmt.Errorf("hash test binary: %w", err)
	}
	for _, arg := range args {
		fmt.Fprintf(hash, "\narg %d %s", len(arg), arg)
	}
	fmt.Fprintf(hash, "\ncpus %d", runtime.NumCPU())
	for _, name := range runtimeEnvironment {
		value := getenv(name)
		fmt.Fprintf(hash, "\nenv %d %s %d %s", len(name), name, len(value), value)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func isTestBinary(path string) bool {
	return strings.HasSuffix(strings.TrimSuffix(filepath.Base(path), ".exe"), ".test")
}

func wrapperEnvironment(root, binary string, getenv func(string) string) Environment {
	env := Environment{Root: filepath.Clean(root)}
	if build, err := filepath.Abs(filepath.Dir(filepath.Dir(binary))); err == nil && strings.HasPrefix(filepath.Base(build), "go-build") {
		env.IgnoreDirs = append(env.IgnoreDirs, build)
	}
	if resolved, err := filepath.EvalSymlinks(env.Root); err == nil && resolved != env.Root {
		env.RootAlias = resolved
	}
	temporary := []string{os.TempDir(), getenv("TMPDIR"), getenv("TMP"), getenv("TEMP"), getenv("GOTMPDIR")}
	if runtime.GOOS != "windows" {
		temporary = append(temporary, "/tmp", "/var/tmp")
	}
	for _, dir := range temporary {
		if dir == "" {
			continue
		}
		dir = filepath.Clean(dir)
		env.TempDirs = append(env.TempDirs, dir)
		if resolved, err := filepath.EvalSymlinks(dir); err == nil && resolved != dir {
			env.TempDirs = append(env.TempDirs, resolved)
		}
	}
	if runtime.GOOS != "windows" {
		env.IgnoreDirs = append(env.IgnoreDirs, "/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/tty", "/dev/stdin", "/dev/stdout", "/dev/stderr", "/dev/fd")
		env.UncacheableDirs = []string{"/dev", "/proc", "/sys"}
	}
	return env
}

func run(args []string, isolate bool, stdin io.Reader, stdout, stderr io.Writer) int {
	return runEnvironment(context.Background(), args, "", os.Environ(), stdin, stdout, stderr)
}

func runEnvironment(ctx context.Context, args []string, directory string, environment []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) // #nosec G204 G702 -- the receipt session executes the requested test binary directly, without a shell, and observes its exit status.
	procgroup.Configure(ctx, cmd)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.Dir = directory
	{
		cmd.Env = slices.DeleteFunc(slices.Clone(environment), func(entry string) bool {
			name, _, _ := strings.Cut(entry, "=")
			return name == SocketEnvironment || name == TokenEnvironment || name == RootEnvironment
		})
	}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", WrapperName, err)
		return 1
	}
	signals := make(chan os.Signal, 4)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for sig := range signals {
			if err := cmd.Process.Signal(sig); err != nil && !errors.Is(err, os.ErrProcessDone) {
				fmt.Fprintf(stderr, "%s: forward %v: %v\n", WrapperName, sig, err)
			}
		}
	}()
	err := cmd.Wait()
	signal.Stop(signals)
	close(signals)
	<-done
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode()
	}
	fmt.Fprintf(stderr, "%s: %v\n", WrapperName, err)
	return 1
}
