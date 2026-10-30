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
	if len(args) >= 3 && args[0] == "--detent-source-digest" {
		if err := writeBinaryDigest(args[1], args[2]); err != nil {
			fmt.Fprintf(stderr, "%s: source verifier: %v\n", WrapperName, err)
			return 1
		}
		return 0
	}
	if len(args) == 0 {
		fmt.Fprintf(stderr, "%s: missing program\n", WrapperName)
		return 2
	}
	socket, token, root := getenv(SocketEnvironment), getenv(TokenEnvironment), getenv(RootEnvironment)
	if !isTestBinary(args[0]) {
		return run(args, false, stdin, stdout, stderr)
	}
	if socket == "" || token == "" || root == "" || !Cacheable(args[1:]) {
		return run(args, true, stdin, stdout, stderr)
	}
	key, err := Key(args[0], args[1:], getenv)
	if err != nil {
		return run(args, true, stdin, stdout, stderr)
	}
	pwd, err := os.Getwd()
	if err != nil {
		return run(args, true, stdin, stdout, stderr)
	}
	env := wrapperEnvironment(root, getenv)
	client := NewClient(socket, token)
	defer client.http.CloseIdleConnections()
	ctx := context.Background()
	candidates, err := client.Lookup(ctx, key)
	if err != nil {
		fmt.Fprintf(stderr, "%s: receipt lookup unavailable: %v\n", WrapperName, err)
	}
	if err := checkEligibility(ctx, args[0]); err != nil {
		fmt.Fprintf(stderr, "%s: receipt not eligible: %v\n", WrapperName, err)
		if err := client.Refuse(ctx, Refusal{Package: packageName(pwd, env), Reason: err.Error()}); err != nil {
			fmt.Fprintf(stderr, "%s: report refusal: %v\n", WrapperName, err)
		}
		return run(args, true, stdin, stdout, stderr)
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

func record(ctx context.Context, client *Client, key, pwd, logPath string, env Environment, start time.Time) error {
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
	binaryHash := sha256.New()
	if err := copyFile(binaryHash, binary); err != nil {
		return "", fmt.Errorf("hash test binary: %w", err)
	}
	fmt.Fprintf(hash, "binary %x", binaryHash.Sum(nil))
	pwd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("working directory: %w", err)
	}
	for _, value := range []string{pwd, getenv(RootEnvironment)} {
		fmt.Fprintf(hash, "\npath %d %s", len(value), value)
	}
	environment := slices.DeleteFunc(os.Environ(), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return name == SocketEnvironment || name == TokenEnvironment || name == RootEnvironment
	})
	for _, entry := range environment {
		fmt.Fprintf(hash, "\nenviron %d %s", len(entry), entry)
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

func wrapperEnvironment(root string, getenv func(string) string) Environment {
	env := Environment{Root: filepath.Clean(root)}
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
		env.UncacheableDirs = []string{"/dev", "/proc", "/sys"}
	}
	return env
}

func run(args []string, isolate bool, stdin io.Reader, stdout, stderr io.Writer) int {
	path, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "%s: locate program: %v\n", WrapperName, err)
		return 1
	}
	cmd := &exec.Cmd{Path: path, Args: slices.Clone(args)}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if isolate {
		cmd.Env = slices.DeleteFunc(os.Environ(), func(entry string) bool {
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
	err = cmd.Wait()
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

func writeBinaryDigest(path, binary string) (err error) {
	hash := sha256.New()
	if err := copyFile(hash, binary); err != nil {
		return fmt.Errorf("hash compiled binary: %w", err)
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create binary digest: %w", err)
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	if _, err := file.Write(hash.Sum(nil)); err != nil {
		return fmt.Errorf("write binary digest: %w", err)
	}
	return nil
}
