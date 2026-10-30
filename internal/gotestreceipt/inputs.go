package gotestreceipt

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

const (
	maxDirectoryEntries = 4096
	maxListedEntries    = 64 << 10
	maxHashedBytes      = 64 << 20
	maxSnapshotEntries  = 64 << 10
	maxSnapshotTime     = 2 * time.Second
	scopeRoot           = "m"
	scopeGoRoot         = "g"
	scopeTemporary      = "t:"
	scopeAbsolute       = "a"
	environInput        = "environ"
)

type pathKind int

const (
	pathInput pathKind = iota
	pathTemporary
	pathUncacheable
)

type Environment struct {
	DigestKey       []byte
	Root            string
	RootAlias       string
	GoRoot          string
	TempDirs        []TempRoot
	TempEntries     map[string]bool
	UncacheableDirs []string
	LookupEnv       func(string) (string, bool)
	Environ         func() []string
}

type TempRoot struct {
	Token string
	Dir   string
}

func (e Environment) lookup(name string) (string, bool) {
	if e.LookupEnv == nil {
		return os.LookupEnv(name)
	}
	return e.LookupEnv(name)
}

func ParseTestLog(r io.Reader, pwd string, env Environment, start time.Time) ([]string, error) {
	seen := make(map[string]struct{})
	add := func(op, path string) error {
		if strings.ContainsRune(path, 0) {
			return nil
		}
		if op == "open" && executable(path) {
			return fmt.Errorf("test started subprocess %s whose inputs are not observable", filepath.Base(path))
		}
		scope, name, kind := env.classify(path)
		switch kind {
		case pathUncacheable:
			return fmt.Errorf("test read system path %s", path)
		case pathTemporary:
			if !env.preexisting(path) {
				return nil
			}
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("temporary input %s disappeared during the test", filepath.Base(path))
			}
			if info.ModTime().After(start) {
				return fmt.Errorf("temporary input %s changed during the test", filepath.Base(path))
			}
		}
		seen[op+"\t"+scope+"\t"+name] = struct{}{}
		return nil
	}
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	first := true
	for scanner.Scan() {
		line := scanner.Text()
		if first {
			first = false
			if line != "# test log" {
				return nil, errors.New("test log header missing")
			}
			continue
		}
		op, name, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("test log line %q is malformed", line)
		}
		var err error
		switch op {
		case "getenv":
			seen["env\t"+name] = struct{}{}
		case "chdir":
			pwd = resolve(pwd, name)
			err = add("stat", pwd)
		case "open", "stat":
			path := resolve(pwd, name)
			if _, statErr := os.Lstat(path); op == "open" && !filepath.IsAbs(name) && errors.Is(statErr, os.ErrNotExist) {
				return nil, fmt.Errorf("test opened missing relative path %s, possibly from another directory", filepath.Base(name))
			}
			err = add(op, path)
		default:
			return nil, fmt.Errorf("test log operation %q is unknown", op)
		}
		if err != nil {
			return nil, err
		}
		if len(seen) > maxLoggedInputs {
			return nil, fmt.Errorf("test read more than %d inputs", maxLoggedInputs)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read test log: %w", err)
	}
	if first {
		return nil, errors.New("test log is empty")
	}
	inputs := make([]string, 0, len(seen))
	for input := range seen {
		inputs = append(inputs, input)
	}
	slices.Sort(inputs)
	inputs = compactInputs(inputs, env)
	if err := ValidateInputs(inputs); err != nil {
		return nil, err
	}
	return inputs, nil
}

type Verifier struct {
	env           Environment
	observed      map[string]observation
	bytes         int64
	entries       int
	hashedContent bool
}

type observation struct {
	value string
	err   error
	path  string
	state pathState
}

type pathState [2]statResult

type statResult struct {
	info    os.FileInfo
	missing bool
}

func NewVerifier(env Environment) *Verifier {
	return &Verifier{env: env, observed: make(map[string]observation), bytes: maxHashedBytes, entries: maxListedEntries}
}

func Digest(inputs []string, env Environment) (string, error) {
	return NewVerifier(env).Digest(inputs)
}

func (v *Verifier) Digest(inputs []string) (string, error) {
	if err := ValidateInputs(inputs); err != nil {
		return "", err
	}
	if len(v.env.DigestKey) != DigestKeyBytes {
		return "", errors.New("receipt digest key is unavailable")
	}
	sorted := slices.Clone(inputs)
	slices.Sort(sorted)
	hash := hmac.New(sha256.New, v.env.DigestKey)
	fmt.Fprintf(hash, "%s\n", protocolFingerprint)
	for _, input := range slices.Compact(sorted) {
		value, err := v.observe(input)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(hash, "%s\t%s\n", input, value)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func ModifiedSince(inputs []string, env Environment, start time.Time) error {
	for _, input := range inputs {
		fields := strings.Split(input, "\t")
		if len(fields) != 3 {
			continue
		}
		path, err := env.path(fields[1], fields[2])
		if err != nil {
			return err
		}
		if fields[0] == treeInput {
			if err := treeModifiedSince(path, fields[2], start); err != nil {
				return err
			}
		}
		for _, stat := range []func(string) (os.FileInfo, error){os.Lstat, os.Stat} {
			info, err := stat(path)
			if errors.Is(err, os.ErrNotExist) {
				if changedAncestor(path, start) {
					return fmt.Errorf("test input %s is missing and its directory changed while the test ran", fields[2])
				}
				continue
			}
			if err != nil {
				return fmt.Errorf("stat test input: %w", err)
			}
			if fields[0] == "stat" && info.IsDir() {
				continue
			}
			if info.ModTime().After(start) {
				return fmt.Errorf("test input %s changed while the test ran", fields[2])
			}
		}
	}
	return nil
}

func (v *Verifier) Match(candidate Candidate) bool {
	digest, err := v.Digest(candidate.Inputs)
	if err != nil || digest != candidate.Digest {
		return false
	}
	for _, input := range candidate.Inputs {
		seen := v.observed[input]
		if seen.path != "" && !seen.state.same(statPath(seen.path)) {
			return false
		}
		if seen.path != "" && strings.HasPrefix(input, "open\t") && strings.HasPrefix(seen.value, "dir ") {
			fields := strings.Split(input, "\t")
			value, err := v.observePath(fields[0], seen.path, strings.HasPrefix(fields[1], scopeTemporary))
			if err != nil || value != seen.value {
				return false
			}
		}
	}
	return true
}

func (v *Verifier) observe(input string) (string, error) {
	if seen, ok := v.observed[input]; ok {
		return seen.value, seen.err
	}
	v.hashedContent = false
	seen := v.observeInput(input)
	if !v.hashedContent {
		v.observed[input] = seen
	}
	return seen.value, seen.err
}

func (v *Verifier) observeInput(input string) observation {
	fields := strings.Split(input, "\t")
	switch {
	case len(fields) == 1 && fields[0] == environInput:
		var entries []string
		for _, entry := range isolatedEnvironment(v.env.environ()) {
			name, value, _ := strings.Cut(entry, "=")
			entries = append(entries, name+"="+v.env.normalize(value))
		}
		return observation{value: "environ " + sum([]byte(strings.Join(entries, "\x00")))}
	case len(fields) == 2 && fields[0] == "env":
		value, ok := v.env.lookup(fields[1])
		if !ok {
			return observation{value: "unset"}
		}
		return observation{value: "set " + sum([]byte(v.env.normalize(value)))}
	case len(fields) == 3 && fields[0] == treeInput:
		path, err := v.env.path(fields[1], fields[2])
		if err != nil {
			return observation{err: err}
		}
		value, err := v.observeTree(path, strings.HasPrefix(fields[1], scopeTemporary))
		return observation{value: value, err: err}
	case len(fields) == 3 && (fields[0] == "open" || fields[0] == "stat"):
		path, err := v.env.path(fields[1], fields[2])
		if err != nil {
			return observation{err: err}
		}
		state := statPath(path)
		value, err := v.observePath(fields[0], path, strings.HasPrefix(fields[1], scopeTemporary))
		return observation{value: value, err: err, path: path, state: state}
	default:
		return observation{err: fmt.Errorf("receipt input %q is malformed", input)}
	}
}

func statPath(path string) pathState {
	var state pathState
	for i, stat := range []func(string) (os.FileInfo, error){os.Lstat, os.Stat} {
		info, err := stat(path)
		state[i] = statResult{info: info, missing: err != nil && errors.Is(err, os.ErrNotExist)}
	}
	return state
}

func (s pathState) same(current pathState) bool {
	for i, before := range s {
		after := current[i]
		if before.info == nil || after.info == nil {
			if before.info != nil || after.info != nil || before.missing != after.missing {
				return false
			}
			continue
		}
		if !os.SameFile(before.info, after.info) || before.info.Size() != after.info.Size() || before.info.Mode() != after.info.Mode() || !before.info.ModTime().Equal(after.info.ModTime()) {
			return false
		}
	}
	return true
}

func (e Environment) path(scope, name string) (string, error) {
	if token, ok := strings.CutPrefix(scope, scopeTemporary); ok {
		for _, root := range e.TempDirs {
			if root.Token == token && validRelativePath(name) {
				return filepath.Join(root.Dir, filepath.FromSlash(name)), nil
			}
		}
		return "", fmt.Errorf("receipt temporary root %q is unavailable", token)
	}
	switch scope {
	case scopeRoot:
		if e.Root == "" || !filepath.IsLocal(filepath.FromSlash(name)) {
			return "", fmt.Errorf("receipt path %q is not local to the module root", name)
		}
		return filepath.Join(e.Root, filepath.FromSlash(name)), nil
	case scopeGoRoot:
		if e.GoRoot == "" || !validRelativePath(name) {
			return "", fmt.Errorf("receipt path %q is not local to GOROOT", name)
		}
		return filepath.Join(e.GoRoot, filepath.FromSlash(name)), nil
	default:
		return "", fmt.Errorf("receipt path scope %q is unknown", scope)
	}
}

func (v *Verifier) observePath(op, path string, host bool) (string, error) {
	info, err := os.Lstat(path)
	if missing, ok := absence(err); ok {
		return missing, nil
	}
	if err != nil {
		return "", fmt.Errorf("stat test input: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("read test input link: %w", err)
		}
		resolved, err := v.observeTarget(op, path, host)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("link %s %s %s", info.Mode().Perm(), sum([]byte(target)), resolved), nil
	}
	return v.describe(op, path, info, host)
}

func (v *Verifier) observeTarget(op, path string, host bool) (string, error) {
	info, err := os.Stat(path)
	if missing, ok := absence(err); ok {
		return missing, nil
	}
	if err != nil {
		return "", fmt.Errorf("stat test input: %w", err)
	}
	return v.describe(op, path, info, host)
}

func absence(err error) (string, bool) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "missing", true
	case errors.Is(err, os.ErrPermission):
		return "denied", true
	}
	return "", false
}

func (v *Verifier) describe(op, path string, info os.FileInfo, host bool) (string, error) {
	switch {
	case info.IsDir():
		if op == "stat" {
			return "dir " + info.Mode().String(), nil
		}
		dir, err := os.Open(path)
		if err != nil {
			return "", fmt.Errorf("open test input directory: %w", err)
		}
		entries, err := v.readDirectory(dir.ReadDir)
		err = errors.Join(err, dir.Close())
		if err != nil {
			return "", fmt.Errorf("list test input: %w", err)
		}
		slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name()+"\x00"+entry.Type().String())
		}
		return "dir " + info.Mode().String() + " " + sum([]byte(strings.Join(names, "\n"))), nil
	case !info.Mode().IsRegular():
		return "", errors.New("test input is a special file")
	case op == "stat" && host:
		return fmt.Sprintf("file %d %s %d", info.Size(), info.Mode(), info.ModTime().UnixNano()), nil
	case op == "stat":
		return fmt.Sprintf("file %d %s", info.Size(), info.Mode()), nil
	}
	if info.Size() > v.bytes {
		return "", errInputBytes
	}
	hash := sha256.New()
	v.hashedContent = true
	if err := v.hashFile(hash, path); err != nil {
		return "", fmt.Errorf("hash test input: %w", err)
	}
	return fmt.Sprintf("file %d %s %s", info.Size(), info.Mode(), hex.EncodeToString(hash.Sum(nil))), nil
}

var (
	errInputBytes   = errors.New("test inputs exceed the verification byte limit")
	errInputEntries = errors.New("test inputs exceed the verification directory entry limit")
)

func (v *Verifier) readDirectory(read func(int) ([]os.DirEntry, error)) ([]os.DirEntry, error) {
	limit := min(maxDirectoryEntries, v.entries)
	entries, err := read(limit + 1)
	if len(entries) > limit {
		return nil, errInputEntries
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	v.entries -= len(entries)
	return entries, nil
}

func (v *Verifier) hashFile(w io.Writer, path string) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	remaining := v.bytes
	n, err := io.Copy(w, io.LimitReader(file, remaining+1))
	v.bytes = remaining - min(n, remaining)
	if err != nil {
		return err
	}
	if n > remaining {
		return errInputBytes
	}
	return nil
}

func copyFile(w io.Writer, path string) (err error) {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	_, err = io.Copy(w, file)
	return err
}

func (e Environment) classify(path string) (string, string, pathKind) {
	for _, dir := range e.UncacheableDirs {
		if within(dir, path) && filepath.Clean(dir) != path {
			return "", "", pathUncacheable
		}
	}
	scope, base, kind := scopeAbsolute, "", pathInput
	for _, root := range e.TempDirs {
		if within(root.Dir, path) && len(root.Dir) > len(base) {
			scope, base, kind = scopeTemporary+root.Token, root.Dir, pathTemporary
		}
	}
	for _, root := range []string{e.Root, e.RootAlias} {
		if within(root, path) && len(root) > len(base) {
			scope, base, kind = scopeRoot, root, pathInput
		}
	}
	if within(e.GoRoot, path) && len(e.GoRoot) > len(base) {
		scope, base, kind = scopeGoRoot, e.GoRoot, pathInput
	}
	if base == "" {
		return scopeAbsolute, path, pathInput
	}
	relative, err := filepath.Rel(base, path)
	if err != nil {
		return scopeAbsolute, path, pathInput
	}
	return scope, filepath.ToSlash(relative), kind
}

func (e Environment) environ() []string {
	if e.Environ == nil {
		return os.Environ()
	}
	return e.Environ()
}

func (e Environment) normalize(value string) string {
	for _, root := range []string{e.Root, e.RootAlias} {
		if root != "" {
			value = strings.ReplaceAll(value, root, fmt.Sprintf("\x00root%d\x00", len(root)))
		}
	}
	for _, root := range e.TempDirs {
		if root.Dir != "" {
			value = strings.ReplaceAll(value, root.Dir, fmt.Sprintf("\x00temp%d\x00", len(root.Dir)))
		}
	}
	return value
}

func (e Environment) preexisting(path string) bool {
	if e.TempEntries == nil {
		return true
	}
	for _, root := range e.TempDirs {
		if !within(root.Dir, path) {
			continue
		}
		relative, err := filepath.Rel(filepath.Clean(root.Dir), path)
		if err != nil {
			return true
		}
		first, _, _ := strings.Cut(filepath.ToSlash(relative), "/")
		if e.TempEntries[filepath.Join(filepath.Clean(root.Dir), first)] {
			return true
		}
	}
	return false
}

func SnapshotTempEntries(roots []TempRoot) (map[string]bool, error) {
	deadline := time.Now().Add(maxSnapshotTime)
	entries := make(map[string]bool)
	for _, root := range roots {
		dir := filepath.Clean(root.Dir)
		file, err := os.Open(dir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("list temporary root: %w", err)
		}
		entries[dir] = true
		for {
			names, err := file.ReadDir(1024)
			for _, name := range names {
				entries[filepath.Join(dir, name.Name())] = true
			}
			if errors.Is(err, io.EOF) {
				break
			}
			if err == nil && (len(entries) > maxSnapshotEntries || time.Now().After(deadline)) {
				err = errors.New("temporary roots exceed the snapshot bound")
			}
			if err != nil {
				return nil, errors.Join(fmt.Errorf("list temporary root: %w", err), file.Close())
			}
		}
		if err := file.Close(); err != nil {
			return nil, fmt.Errorf("list temporary root: %w", err)
		}
	}
	return entries, nil
}

func executable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS != "windows" {
		return info.Mode().Perm()&0o111 != 0
	}
	extensions := os.Getenv("PATHEXT")
	if extensions == "" {
		extensions = ".com;.exe;.bat;.cmd"
	}
	for _, extension := range strings.Split(extensions, ";") {
		if extension != "" && strings.EqualFold(filepath.Ext(path), extension) {
			return true
		}
	}
	return false
}

func changedAncestor(path string, start time.Time) bool {
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		info, err := os.Stat(dir)
		if err == nil {
			return info.ModTime().After(start)
		}
		if parent := filepath.Dir(dir); parent == dir {
			return true
		}
	}
}

func resolve(pwd, name string) string {
	if !filepath.IsAbs(name) {
		name = filepath.Join(pwd, name)
	}
	return filepath.Clean(name)
}

func within(dir, path string) bool {
	if dir == "" {
		return false
	}
	relative, err := filepath.Rel(filepath.Clean(dir), path)
	return err == nil && (relative == "." || filepath.IsLocal(relative))
}

func sum(data []byte) string {
	value := sha256.Sum256(data)
	return hex.EncodeToString(value[:])
}
