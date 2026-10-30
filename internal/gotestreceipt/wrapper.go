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

const testWaitDelay = 10 * time.Second

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
	socket, root := getenv(SocketEnvironment), getenv(RootEnvironment)
	if socket != "" && root != "" && isTestBinary(args[0]) {
		request, err := wrapperRequest(args, root)
		if err == nil {
			var code int
			code, err = requestExecution(socket, request, stdout, stderr)
			if err == nil {
				return code
			}
			if errors.Is(err, errRunLocally) {
				return run(args, stdin, stdout, stderr)
			}
			if !errors.Is(err, errExecutionUnavailable) {
				fmt.Fprintf(stderr, "%s: %v\n", WrapperName, err)
				return code
			}
		}
		fmt.Fprintf(stderr, "%s: receipt session unavailable; running test binary: %v\n", WrapperName, err)
	}
	return run(args, stdin, stdout, stderr)
}

func wrapperRequest(args []string, root string) (executionRequest, error) {
	binary, err := filepath.Abs(args[0])
	if err != nil {
		return executionRequest{}, fmt.Errorf("resolve test binary: %w", err)
	}
	pwd, err := os.Getwd()
	if err != nil {
		return executionRequest{}, fmt.Errorf("resolve test directory: %w", err)
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return executionRequest{}, fmt.Errorf("resolve module root: %w", err)
	}
	return executionRequest{Args: append([]string{binary}, args[1:]...), Directory: pwd, Root: root, Environment: isolatedEnvironment(os.Environ())}, nil
}

func (s *Session) executableTestRequest(request executionRequest) bool {
	return len(request.Args) > 0 && isTestBinary(request.Args[0]) && s.builtBinary(request.Args[0]) && filepath.Clean(request.Root) == s.root && within(s.root, filepath.Clean(request.Directory))
}

func (s *Session) builtBinary(binary string) bool {
	relative, err := filepath.Rel(s.workDir, filepath.Clean(binary))
	if err != nil || !filepath.IsLocal(relative) {
		return false
	}
	first, rest, _ := strings.Cut(filepath.ToSlash(relative), "/")
	return strings.HasPrefix(first, "go-build") && rest != ""
}

func (s *Session) executeTest(ctx context.Context, request executionRequest, stdout, stderr io.Writer) (int, bool) {
	if !Cacheable(request.Args[1:]) {
		return 0, true
	}
	private, err := os.MkdirTemp(s.dir, "bin")
	if err != nil {
		return 0, true
	}
	defer func() {
		if err := os.RemoveAll(private); err != nil {
			fmt.Fprintf(stderr, "%s: remove private test binary: %v\n", WrapperName, err)
		}
	}()
	binary := filepath.Join(private, filepath.Base(request.Args[0]))
	if err := copyExecutable(binary, request.Args[0]); err != nil {
		return 0, true
	}
	request.Args = append([]string{binary}, request.Args[1:]...)
	request.Root = s.root
	args := request.Args
	key, err := request.key()
	if err != nil {
		return 0, true
	}
	attestation, err := s.attest(ctx)
	if err != nil {
		fmt.Fprintf(stderr, "%s: %v\n", WrapperName, err)
		return 0, true
	}
	env := wrapperEnvironment(s.root, s.goRoot(ctx), request.getenv)
	env.LookupEnv = request.lookup
	env.DigestKey = attestation.DigestKey
	candidates, err := s.lookup(ctx, key)
	if err != nil {
		fmt.Fprintf(stderr, "%s: receipt lookup unavailable: %v\n", WrapperName, err)
	}
	if err := checkEligibility(ctx, args[0], request.Directory, s.dir, request.Environment); err != nil {
		fmt.Fprintf(stderr, "%s: receipt not eligible: %v\n", WrapperName, err)
		s.refuse(packageName(request.Directory, env), err)
		return 0, true
	}
	verifier := NewVerifier(env)
	for _, candidate := range candidates {
		if verifier.Match(candidate) {
			s.hits.Add(1)
			fmt.Fprintln(stdout, "PASS")
			return 0, false
		}
	}
	logFile, err := os.CreateTemp(s.dir, "detent-test-log-*")
	if err != nil {
		return 0, true
	}
	logPath := logFile.Name()
	defer func() {
		if err := os.Remove(logPath); err != nil {
			fmt.Fprintf(stderr, "%s: remove test log: %v\n", WrapperName, err)
		}
	}()
	if err := logFile.Close(); err != nil {
		return 0, true
	}
	start := time.Now()
	code := runTest(ctx, request, append(slices.Clone(args), "-test.testlogfile="+logPath), stdout, stderr)
	if code != 0 {
		return code, false
	}
	if err := s.recordRun(ctx, key, request, logPath, env, start); err != nil {
		fmt.Fprintf(stderr, "%s: receipt not recorded: %v\n", WrapperName, err)
		s.refuse(packageName(request.Directory, env), err)
	}
	return code, false
}

func (s *Session) recordRun(ctx context.Context, key string, request executionRequest, logPath string, env Environment, start time.Time) error {
	if after, err := request.key(); err != nil || after != key {
		return errors.New("test binary changed while the test ran")
	}
	inputs, err := readTestLog(logPath, request.Directory, env, start)
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
	return s.record(ctx, Receipt{Key: key, Package: packageName(request.Directory, env), Inputs: inputs, Digest: digest})
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

func (r executionRequest) key() (string, error) {
	return Key(r.Args[0], r.Args[1:], r.Directory, r.Root, r.Environment)
}

func Key(binary string, args []string, pwd, root string, environ []string) (string, error) {
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\n%s/%s\n", protocolFingerprint, runtime.GOOS, runtime.GOARCH)
	binaryHash := sha256.New()
	if err := copyFile(binaryHash, binary); err != nil {
		return "", fmt.Errorf("hash test binary: %w", err)
	}
	fmt.Fprintf(hash, "binary %x", binaryHash.Sum(nil))
	for _, value := range []string{pwd, root} {
		fmt.Fprintf(hash, "\npath %d %s", len(value), value)
	}
	environment := isolatedEnvironment(environ)
	values := make(map[string]string, len(environment))
	for _, entry := range environment {
		fmt.Fprintf(hash, "\nenviron %d %s", len(entry), entry)
		name, value, _ := strings.Cut(entry, "=")
		values[name] = value
	}
	for _, arg := range args {
		fmt.Fprintf(hash, "\narg %d %s", len(arg), arg)
	}
	fmt.Fprintf(hash, "\ncpus %d", runtime.NumCPU())
	for _, name := range runtimeEnvironment {
		value := values[name]
		fmt.Fprintf(hash, "\nenv %d %s %d %s", len(name), name, len(value), value)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func isTestBinary(path string) bool {
	return strings.HasSuffix(strings.TrimSuffix(filepath.Base(path), ".exe"), ".test")
}

func wrapperEnvironment(root, goRoot string, getenv func(string) string) Environment {
	env := Environment{Root: filepath.Clean(root), GoRoot: goRoot}
	if resolved, err := filepath.EvalSymlinks(env.Root); err == nil && resolved != env.Root {
		env.RootAlias = resolved
	}
	temporary := []TempRoot{{"system", os.TempDir()}, {"TMPDIR", getenv("TMPDIR")}, {"TMP", getenv("TMP")}, {"TEMP", getenv("TEMP")}, {"GOTMPDIR", getenv("GOTMPDIR")}}
	if runtime.GOOS != "windows" {
		temporary = append(temporary, TempRoot{"tmp", "/tmp"}, TempRoot{"vartmp", "/var/tmp"})
	}
	for _, root := range temporary {
		if root.Dir == "" {
			continue
		}
		root.Dir = filepath.Clean(root.Dir)
		env.TempDirs = append(env.TempDirs, root)
		if resolved, err := filepath.EvalSymlinks(root.Dir); err == nil && resolved != root.Dir {
			env.TempDirs = append(env.TempDirs, TempRoot{root.Token, resolved})
		}
	}
	if runtime.GOOS != "windows" {
		env.UncacheableDirs = []string{"/dev", "/proc", "/sys"}
	}
	return env
}

func isolatedEnvironment(environment []string) []string {
	return slices.DeleteFunc(slices.Clone(environment), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return name == SocketEnvironment || name == RootEnvironment || name == "GOTMPDIR"
	})
}

func runTest(ctx context.Context, request executionRequest, args []string, stdout, stderr io.Writer) int {
	cmd := &exec.Cmd{Path: args[0], Args: slices.Clone(args), Dir: request.Directory, Env: isolatedEnvironment(request.Environment), Stdout: stdout, Stderr: stderr, WaitDelay: testWaitDelay}
	procgroup.Configure(ctx, cmd)
	terminate := cmd.Cancel
	cmd.Cancel = nil
	if err := procgroup.Start(cmd); err != nil {
		return exitCode(err, stderr)
	}
	stop := context.AfterFunc(ctx, func() {
		if err := terminate(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			fmt.Fprintf(stderr, "%s: stop test binary: %v\n", WrapperName, err)
		}
	})
	err := procgroup.Wait(cmd)
	stop()
	return exitCode(err, stderr)
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	path, err := exec.LookPath(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "%s: locate program: %v\n", WrapperName, err)
		return 1
	}
	cmd := &exec.Cmd{Path: path, Args: slices.Clone(args), Env: isolatedEnvironment(os.Environ())}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
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
	return exitCode(err, stderr)
}

func exitCode(err error, stderr io.Writer) int {
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

func copyExecutable(destination, source string) (err error) {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open test binary: %w", err)
	}
	defer func() { err = errors.Join(err, input.Close()) }()
	output, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private test binary: %w", err)
	}
	if err := output.Chmod(0o700); err != nil {
		return errors.Join(fmt.Errorf("mark private test binary executable: %w", err), output.Close())
	}
	if _, err := io.Copy(output, input); err != nil {
		return errors.Join(fmt.Errorf("copy test binary: %w", err), output.Close())
	}
	return output.Close()
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
