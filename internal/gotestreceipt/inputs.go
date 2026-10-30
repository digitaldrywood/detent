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
	"slices"
	"strings"
	"time"
)

const (
	scopeRoot      = "m"
	scopeTemporary = "t"
	scopeAbsolute  = "a"
)

type pathKind int

const (
	pathInput pathKind = iota
	pathTemporary
	pathIgnored
	pathUncacheable
)

type Environment struct {
	DigestKey       []byte
	Root            string
	RootAlias       string
	TempDirs        []string
	IgnoreDirs      []string
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
		case pathIgnored:
			return nil
		case pathUncacheable:
			return fmt.Errorf("test read system path %s", path)
		case pathTemporary:
			info, err := os.Lstat(path)
			if err != nil || info.ModTime().After(start) {
				return nil
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

func Digest(inputs []string, env Environment) (string, error) {
	if err := ValidateInputs(inputs); err != nil {
		return "", err
	}
	sorted := slices.Clone(inputs)
	slices.Sort(sorted)
	hash := sha256.New()
	fmt.Fprintf(hash, "%s\n", protocolFingerprint)
	for _, input := range slices.Compact(sorted) {
		value, err := env.observe(input)
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

func (e Environment) observe(input string) (string, error) {
	fields := strings.Split(input, "\t")
	switch {
	case len(fields) == 2 && fields[0] == "env":
		value, ok := e.lookup(fields[1])
		if !ok {
			return "unset", nil
		}
		if len(e.DigestKey) < 32 {
			return "", errors.New("environment digest key is unavailable")
		}
		hash := hmac.New(sha256.New, e.DigestKey)
		fmt.Fprintf(hash, "%s\x00%s", fields[1], e.normalize(value))
		return "set " + hex.EncodeToString(hash.Sum(nil)), nil
	case len(fields) == 3 && (fields[0] == "open" || fields[0] == "stat"):
		path, err := e.path(fields[1], fields[2])
		if err != nil {
			return "", err
		}
		return observePath(fields[0], path)
	default:
		return "", fmt.Errorf("receipt input %q is malformed", input)
	}
}

func (e Environment) path(scope, name string) (string, error) {
	switch scope {
	case scopeTemporary:
		if !validRelativePath(name) || len(e.TempDirs) == 0 {
			return "", errors.New("invalid temporary input")
		}
		for _, root := range e.TempDirs {
			candidate := filepath.Join(root, filepath.FromSlash(name))
			if _, err := os.Lstat(candidate); err == nil {
				return candidate, nil
			}
		}
		return filepath.Join(e.TempDirs[0], filepath.FromSlash(name)), nil
	case scopeRoot:
		if e.Root == "" || !filepath.IsLocal(filepath.FromSlash(name)) {
			return "", fmt.Errorf("receipt path %q is not local to the module root", name)
		}
		return filepath.Join(e.Root, filepath.FromSlash(name)), nil
	default:
		return "", fmt.Errorf("receipt path scope %q is unknown", scope)
	}
}

func observePath(op, path string) (string, error) {
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
		resolved, err := observeTarget(op, path)
		if err != nil {
			return "", err
		}
		return "link " + sum([]byte(target)) + " " + resolved, nil
	}
	return describe(op, path, info)
}

func observeTarget(op, path string) (string, error) {
	info, err := os.Stat(path)
	if missing, ok := absence(err); ok {
		return missing, nil
	}
	if err != nil {
		return "", fmt.Errorf("stat test input: %w", err)
	}
	return describe(op, path, info)
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

func describe(op, path string, info os.FileInfo) (string, error) {
	switch {
	case info.IsDir():
		if op == "stat" {
			return "dir " + info.Mode().Perm().String(), nil
		}
		entries, err := os.ReadDir(path)
		if err != nil {
			return "", fmt.Errorf("list test input: %w", err)
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name()+"\x00"+entry.Type().String())
		}
		return "dir " + info.Mode().Perm().String() + " " + sum([]byte(strings.Join(names, "\n"))), nil
	case !info.Mode().IsRegular():
		return "special " + info.Mode().String(), nil
	case op == "stat":
		return fmt.Sprintf("file %d %s", info.Size(), info.Mode().Perm()), nil
	}
	hash := sha256.New()
	if err := copyFile(hash, path); err != nil {
		return "", fmt.Errorf("hash test input: %w", err)
	}
	return fmt.Sprintf("file %s %s", info.Mode().Perm(), hex.EncodeToString(hash.Sum(nil))), nil
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
	for _, dir := range e.IgnoreDirs {
		if within(dir, path) {
			return "", "", pathIgnored
		}
	}
	for _, dir := range e.UncacheableDirs {
		if within(dir, path) && filepath.Clean(dir) != path {
			return "", "", pathUncacheable
		}
	}
	temporary := ""
	for _, dir := range e.TempDirs {
		if dir != "" && filepath.Clean(dir) == path {
			return "", "", pathIgnored
		}
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
		relative, err := filepath.Rel(temporary, path)
		if err == nil {
			return scopeTemporary, filepath.ToSlash(relative), pathTemporary
		}
	}
	return scopeAbsolute, path, pathInput
}

func (e Environment) normalize(value string) string {
	for _, root := range []string{e.Root, e.RootAlias} {
		if root != "" {
			value = strings.ReplaceAll(value, root, "\x00root")
		}
	}
	for _, dir := range e.TempDirs {
		if dir != "" {
			value = strings.ReplaceAll(value, dir, "\x00temp")
		}
	}
	return value
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
