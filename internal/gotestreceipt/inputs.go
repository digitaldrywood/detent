package gotestreceipt

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	maxDirectoryEntries = 4096
	maxListedEntries    = 64 << 10
	maxHashedBytes      = 64 << 20
	scopeRoot           = "m"
	scopeAbsolute       = "a"
)

type pathKind int

const (
	pathInput pathKind = iota
	pathTemporary
	pathUncacheable
)

type Environment struct {
	Root            string
	RootAlias       string
	TempDirs        []string
	UncacheableDirs []string
	LookupEnv       func(string) (string, bool)
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
		scope, name, kind := env.classify(path)
		switch kind {
		case pathUncacheable:
			return fmt.Errorf("test read system path %s", path)
		case pathTemporary:
			info, err := os.Lstat(path)
			if err != nil {
				return fmt.Errorf("temporary input unavailable: %w", err)
			}
			if info.ModTime().After(start) {
				return errors.New("temporary input changed during test")
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
			err = add(op, resolve(pwd, name))
		default:
			return nil, fmt.Errorf("test log operation %q is unknown", op)
		}
		if err != nil {
			return nil, err
		}
		if len(seen) > MaxInputs {
			return nil, fmt.Errorf("test read more than %d inputs", MaxInputs)
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
	sorted := slices.Clone(inputs)
	slices.Sort(sorted)
	hash := sha256.New()
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
		for _, stat := range []func(string) (os.FileInfo, error){os.Lstat, os.Stat} {
			info, err := stat(path)
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("test input is missing after execution")
			}
			if err != nil {
				return fmt.Errorf("stat test input: %w", err)
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
	case len(fields) == 2 && fields[0] == "env":
		value, ok := v.env.lookup(fields[1])
		if !ok {
			return observation{value: "unset"}
		}
		return observation{value: "set " + sum([]byte(value))}
	case len(fields) == 3 && (fields[0] == "open" || fields[0] == "stat"):
		path, err := v.env.path(fields[1], fields[2])
		if err != nil {
			return observation{err: err}
		}
		state := statPath(path)
		value, err := v.observePath(fields[0], path)
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
	switch scope {
	case scopeRoot:
		if e.Root == "" || !filepath.IsLocal(filepath.FromSlash(name)) {
			return "", fmt.Errorf("receipt path %q is not local to the module root", name)
		}
		return filepath.Join(e.Root, filepath.FromSlash(name)), nil
	case scopeAbsolute:
		if !filepath.IsAbs(name) {
			return "", fmt.Errorf("receipt path %q is not absolute", name)
		}
		return name, nil
	default:
		return "", fmt.Errorf("receipt path scope %q is unknown", scope)
	}
}

func (v *Verifier) observePath(op, path string) (string, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("test input is missing")
	}
	if err != nil {
		return "", fmt.Errorf("stat test input: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(path)
		if err != nil {
			return "", fmt.Errorf("read test input link: %w", err)
		}
		resolved, err := v.observeTarget(op, path)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("link %s %d %v %s %s", info.Mode(), info.ModTime().UnixNano(), info.Sys(), sum([]byte(target)), resolved), nil
	}
	return v.describe(op, path, info)
}

func (v *Verifier) observeTarget(op, path string) (string, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", errors.New("test input is missing")
	}
	if err != nil {
		return "", fmt.Errorf("stat test input: %w", err)
	}
	return v.describe(op, path, info)
}

func (v *Verifier) describe(op, path string, info os.FileInfo) (string, error) {
	switch {
	case info.IsDir():
		if op == "stat" {
			return fmt.Sprintf("dir %s %d %v", info.Mode(), info.ModTime().UnixNano(), info.Sys()), nil
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
		return fmt.Sprintf("dir %s %d %v", info.Mode(), info.ModTime().UnixNano(), info.Sys()) + " " + sum([]byte(strings.Join(names, "\n"))), nil
	case !info.Mode().IsRegular():
		return "", errors.New("test input is a special file")
	case op == "stat":
		return fmt.Sprintf("file %d %s %d %v", info.Size(), info.Mode(), info.ModTime().UnixNano(), info.Sys()), nil
	}
	if info.Size() > v.bytes {
		return "", errInputBytes
	}
	hash := sha256.New()
	v.hashedContent = true
	if err := v.hashFile(hash, path); err != nil {
		return "", fmt.Errorf("hash test input: %w", err)
	}
	return fmt.Sprintf("file %d %s %d %v %s", info.Size(), info.Mode(), info.ModTime().UnixNano(), info.Sys(), hex.EncodeToString(hash.Sum(nil))), nil
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
	temporary := ""
	for _, dir := range e.TempDirs {
		if within(dir, path) && len(dir) > len(temporary) {
			temporary = dir
		}
	}
	for _, root := range []string{e.Root, e.RootAlias} {
		if within(root, path) && len(root) > len(temporary) {
			relative, err := filepath.Rel(root, path)
			if err == nil {
				return scopeRoot, filepath.ToSlash(relative), pathInput
			}
		}
	}
	if temporary != "" {
		return scopeAbsolute, path, pathTemporary
	}
	return scopeAbsolute, path, pathInput
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
