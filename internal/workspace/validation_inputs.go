package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/mod/modfile"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/gate"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

var landingValidationRecipe = regexp.MustCompile(`^bash scripts/check-landing\.sh "([^"\r\n]+)" "([1-9][0-9]*)" "([^"\r\n]+)"$`)
var generatedGoBuildPrefixMap = regexp.MustCompile(`(^|[ \t])-ffile-prefix-map=[^ \t=]+[/\\]go-build[0-9]+=([^ \t]+)([ \t]|$)`)

func reviewCommandEvidence(ctx context.Context, command *exec.Cmd, validation string) *gate.CommandEvidence {
	sourceBound := true
	module, moduleErr := os.ReadFile(filepath.Join(command.Dir, "go.mod"))
	if moduleErr == nil {
		parsed, err := modfile.Parse("go.mod", module, nil)
		if err != nil {
			return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
		}
		for _, replacement := range parsed.Replace {
			if replacement.New.Version == "" {
				sourceBound = false
			}
		}
	} else if !os.IsNotExist(moduleErr) {
		return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	}
	trackedEntries, err := runGitAtWithEnv(ctx, command.Dir, command.Env, "ls-files", "--stage", "-z")
	if err != nil {
		return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	}
	for _, entry := range strings.Split(trackedEntries, "\x00") {
		if strings.HasPrefix(entry, "160000 ") {
			sourceBound = false
		}
	}
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "GOFLAGS" {
			if !reusableGoValidationFlags(strings.Fields(value)) {
				return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
			}
		}
	}
	if directGoValidation(validation) {
		packageInputs := strings.Fields(validation)[1] != "version"
		evidence := validationEnvironment(ctx, command, packageInputs)
		if !sourceBound {
			evidence.InputsSHA = ""
		}
		if packageInputs && evidence.InputsSHA != "" {
			ignored, err := runGitAtWithEnv(ctx, command.Dir, command.Env, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
			if err != nil || ignored != "" {
				evidence.InputsSHA = ""
			}
		}
		return evidence
	}
	evidence := &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	if validation != "make check-landing" {
		return evidence
	}
	entries, err := os.ReadDir(command.Dir)
	if err != nil {
		return evidence
	}
	for _, entry := range entries {
		if entry.Name() == "GNUmakefile" || entry.Name() == "makefile" {
			return evidence
		}
	}
	for _, name := range []string{"Makefile", "scripts/check-landing.sh", "scripts/check-evidence.sh"} {
		actual, err := os.ReadFile(filepath.Join(command.Dir, name))
		if err != nil {
			return evidence
		}
		supported, err := fs.ReadFile(detent.ValidationFS(), name)
		if err != nil || !bytes.Equal(actual, supported) {
			return evidence
		}
	}
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		if value != "" && slices.Contains([]string{"MAKEFILES", "MAKEFLAGS", "GNUMAKEFLAGS", "MFLAGS", "BASH_ENV", "ENV"}, key) {
			return evidence
		}
	}
	resolve := validationProbe(ctx, command, "make --no-print-directory -n check-landing")
	if resolve == nil {
		return evidence
	}
	output, err := resolve.Output()
	if err != nil {
		return evidence
	}
	args := landingValidationRecipe.FindStringSubmatch(strings.TrimSpace(string(output)))
	if len(args) != 4 {
		return evidence
	}
	base, err := runGitAtWithEnv(ctx, command.Dir, command.Env, "rev-parse", "--verify", args[1]+"^{commit}")
	if err != nil {
		return evidence
	}
	if moduleErr != nil {
		return evidence
	}
	toolchain := "auto"
	found := false
	for _, line := range strings.Split(string(module), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && fields[0] == "toolchain" {
			if found {
				return evidence
			}
			found = true
			toolchain = fields[1]
		}
	}
	probe := *command
	probe.Env = append(slices.Clone(command.Env), "GOTOOLCHAIN="+toolchain, "GOMAXPROCS=1")
	evidence = validationEnvironment(ctx, &probe, true)
	if !evidence.Environment.Known() {
		return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	}
	if evidence.InputsSHA == "" {
		return evidence
	}
	if !sourceBound {
		evidence.InputsSHA = ""
		return evidence
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(evidence.InputsSHA + "\n" + strings.ReplaceAll(string(output), command.Dir, "<workspace>") + "\n" + strings.TrimSpace(base)))
	linter := args[3]
	if !filepath.IsAbs(linter) {
		linter = filepath.Join(command.Dir, linter)
	}
	paths := []string{linter, "/bin/bash"}
	for _, name := range []string{"go", "make", "bash", "node", "npm", "python3", "git", "awk", "env"} {
		path, err := validationExecutable(ctx, &probe, name)
		if err != nil {
			return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
		}
		paths = append(paths, path)
	}
	for _, path := range paths {
		file, err := os.Open(path)
		if err != nil {
			return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
		}
		info, statErr := file.Stat()
		identity := sha256.New()
		_, copyErr := io.Copy(identity, file)
		closeErr := file.Close()
		if statErr != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 || copyErr != nil || closeErr != nil {
			return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
		}
		_, _ = digest.Write(identity.Sum(nil))
	}
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		if value != "" && (key == "APP_DIR" && value != "web/conversation" || key == "TAILWIND_INPUT" && value != "static/css/input.css") {
			evidence.InputsSHA = ""
			return evidence
		}
	}
	if _, err := os.Stat(filepath.Join(command.Dir, "static", "css", "input.css")); err == nil {
		identity, err := validationDirectoryIdentity(filepath.Join(command.Dir, "node_modules"))
		if err != nil {
			return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
		}
		_, _ = digest.Write(identity)
	} else if !os.IsNotExist(err) {
		return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	}

	for _, name := range []string{"web", "web/conversation"} {
		info, err := os.Lstat(filepath.Join(command.Dir, filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() {
			evidence.InputsSHA = ""
			return evidence
		}
	}
	tracked := validationProbe(ctx, command, "git ls-files --stage -z -- web/conversation")
	if tracked == nil {
		evidence.InputsSHA = ""
		return evidence
	}
	files, err := tracked.Output()
	if err != nil {
		evidence.InputsSHA = ""
		return evidence
	}
	for _, file := range strings.Split(string(files), "\x00") {
		if strings.HasPrefix(file, "120000 ") {
			evidence.InputsSHA = ""
			return evidence
		}
	}
	ignored := validationProbe(ctx, command, "git status --porcelain=v1 --ignored=matching -z -- web/conversation")
	if ignored == nil {
		evidence.InputsSHA = ""
		return evidence
	}
	files, err = ignored.Output()
	if err != nil {
		evidence.InputsSHA = ""
		return evidence
	}
	for _, file := range strings.Split(string(files), "\x00") {
		if strings.HasPrefix(file, "!! ") && !strings.HasPrefix(file, "!! web/conversation/node_modules/") {
			evidence.InputsSHA = ""
			return evidence
		}
	}
	for _, name := range []string{"web/conversation/package.json", "web/conversation/package-lock.json"} {
		content, err := os.ReadFile(filepath.Join(command.Dir, filepath.FromSlash(name)))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			evidence.InputsSHA = ""
			return evidence
		}
		var value any
		if json.Unmarshal(content, &value) != nil || validationLocalNodeSource(value) {
			evidence.InputsSHA = ""
			return evidence
		}
	}
	npm, err := validationExecutable(ctx, &probe, "npm")
	if err != nil {
		evidence.InputsSHA = ""
		return evidence
	}
	npm, err = filepath.EvalSymlinks(npm)
	if err != nil {
		evidence.InputsSHA = ""
		return evidence
	}
	npmRoot := filepath.Dir(filepath.Dir(npm))
	content, err := os.ReadFile(filepath.Join(npmRoot, "package.json"))
	var npmPackage struct{ Name string }
	if err != nil || json.Unmarshal(content, &npmPackage) != nil || npmPackage.Name != "npm" {
		evidence.InputsSHA = ""
		return evidence
	}
	identity, err := validationDirectoryIdentity(npmRoot)
	if err != nil {
		evidence.InputsSHA = ""
		return evidence
	}
	_, _ = digest.Write(identity)
	for _, name := range []string{"userconfig", "globalconfig"} {
		config := validationProbe(ctx, &probe, "npm config get "+name)
		if config == nil {
			evidence.InputsSHA = ""
			return evidence
		}
		output, err := config.Output()
		path := strings.TrimSpace(string(output))
		if err != nil || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n") {
			evidence.InputsSHA = ""
			return evidence
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			evidence.InputsSHA = ""
			return evidence
		}
	}
	ignoredIdentity, err := validationIgnoredInputs(ctx, command, paths)
	if err != nil {
		evidence.InputsSHA = ""
		return evidence
	}
	_, _ = digest.Write(ignoredIdentity)
	evidence.InputsSHA = hex.EncodeToString(digest.Sum(nil))
	return evidence
}

func validationIgnoredInputs(ctx context.Context, command *exec.Cmd, executables []string) (identity []byte, resultErr error) {
	ignored, err := runGitAtWithEnv(ctx, command.Dir, command.Env, "ls-files", "--others", "--ignored", "--exclude-standard", "-z")
	if err != nil {
		return nil, err
	}
	boundExecutables := make(map[string]bool)
	for _, executable := range executables {
		path, err := filepath.Rel(command.Dir, executable)
		if err != nil {
			return nil, err
		}
		boundExecutables[filepath.ToSlash(path)] = true
	}
	paths := []struct {
		name      string
		directory bool
	}{{"node_modules", true}, {"web/conversation/node_modules", true}, {"static/app/conversation", true}, {"static/css/output.css", false}, {"docs/config.md", false}, {"config.reference.yaml", false}}
	for _, name := range strings.Split(ignored, "\x00") {
		if name == "" || boundExecutables[name] {
			continue
		}
		bound := false
		for _, path := range paths {
			if name == path.name || path.directory && strings.HasPrefix(name, path.name+"/") {
				bound = true
				break
			}
		}
		if !bound {
			return nil, fs.ErrInvalid
		}
	}
	root, err := os.OpenRoot(command.Dir)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	digest := sha256.New()
	for _, path := range paths {
		ancestor := ""
		for _, component := range strings.Split(path.name, "/") {
			ancestor = filepath.Join(ancestor, component)
			info, err := root.Lstat(ancestor)
			if os.IsNotExist(err) {
				break
			}
			if err != nil {
				return nil, err
			}
			if info.Mode()&os.ModeSymlink != 0 || ancestor != filepath.FromSlash(path.name) && !info.IsDir() {
				return nil, fs.ErrInvalid
			}
		}
		info, err := root.Lstat(path.name)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var contentIdentity []byte
		if path.directory && info.IsDir() {
			var child *os.Root
			child, err = root.OpenRoot(path.name)
			if err == nil {
				contentIdentity, err = validationRootIdentity(child)
				err = errors.Join(err, child.Close())
			}
		} else if !path.directory && info.Mode().IsRegular() {
			var content []byte
			content, err = root.ReadFile(path.name)
			value := sha256.Sum256(content)
			contentIdentity = value[:]
		} else {
			return nil, fs.ErrInvalid
		}
		if err != nil {
			return nil, err
		}
		if err := json.NewEncoder(digest).Encode(struct{ Path, Mode, SHA string }{path.name, info.Mode().String(), hex.EncodeToString(contentIdentity)}); err != nil {
			return nil, err
		}
	}
	return digest.Sum(nil), nil
}

func validationInputs(ctx context.Context, command *exec.Cmd, observed []byte, packageInputs bool) string {
	var goEnvironment map[string]string
	if json.Unmarshal(observed, &goEnvironment) != nil {
		return ""
	}
	if packageInputs && goEnvironment["CGO_ENABLED"] != "0" {
		return ""
	}
	identities := make(map[string]string)
	if len(command.Args) < 2 {
		return ""
	}
	identities["shell_args"] = strings.Join(command.Args[:len(command.Args)-1], "\x00")
	launcher, err := validationExecutable(ctx, command, "go")
	if err != nil {
		return ""
	}
	paths := []string{command.Path, launcher}
	suffix := ""
	if goEnvironment["GOHOSTOS"] == "windows" {
		suffix = ".exe"
	}
	if root, tools := goEnvironment["GOROOT"], goEnvironment["GOTOOLDIR"]; root != "" && tools != "" {
		paths = append(paths, filepath.Join(root, "bin", "go"+suffix))
		for _, name := range []string{"compile", "asm", "link", "cgo", "vet"} {
			paths = append(paths, filepath.Join(tools, name+suffix))
		}
	} else {
		return ""
	}
	if goEnvironment["CGO_ENABLED"] == "1" {
		for _, key := range []string{"CC", "CXX"} {
			name := goEnvironment[key]
			if name == "" || len(strings.Fields(name)) != 1 {
				return ""
			}
			path, err := validationExecutable(ctx, command, name)
			if err != nil {
				return ""
			}
			paths = append(paths, path)
		}
	}
	for _, path := range paths {
		info, err := os.Stat(path)
		if err != nil || !info.Mode().IsRegular() || suffix == "" && info.Mode().Perm()&0o111 == 0 {
			return ""
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return ""
		}
		digest := sha256.Sum256(content)
		identities[strings.ReplaceAll(path, command.Dir, "<workspace>")] = info.Mode().String() + ":" + hex.EncodeToString(digest[:])
	}
	sdk, err := validationDirectoryIdentity(goEnvironment["GOROOT"])
	if err != nil {
		return ""
	}
	identities["sdk_sha"] = hex.EncodeToString(sdk)
	for key, value := range goEnvironment {
		if key == "GOFLAGS" {
			if !reusableGoValidationFlags(strings.Fields(value)) {
				return ""
			}
		}
		if slices.Contains([]string{"CGO_CFLAGS", "CGO_CPPFLAGS", "CGO_CXXFLAGS", "CGO_FFLAGS", "CGO_LDFLAGS"}, key) {
			for _, flag := range strings.Fields(value) {
				switch flag {
				case "-O", "-O0", "-O1", "-O2", "-O3", "-Os", "-Oz", "-Og", "-Ofast", "-g", "-g0", "-g1", "-g2", "-g3", "-ggdb", "-ggdb1", "-ggdb2", "-ggdb3":
				default:
					return ""
				}
			}
		}
		if key == "GOWORK" && value != "" && value != "off" {
			return ""
		}
		if key == "GOTMPDIR" {
			delete(goEnvironment, key)
			continue
		}
		if key == "GOGCCFLAGS" {
			value = generatedGoBuildPrefixMap.ReplaceAllString(value, `${1}-ffile-prefix-map=<go-build>=${2}${3}`)
		}
		goEnvironment[key] = strings.ReplaceAll(value, command.Dir, "<workspace>")
	}
	canonical, err := json.Marshal(goEnvironment)
	if err != nil {
		return ""
	}
	values := make(map[string]string)
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		if slices.Contains([]string{"TMPDIR", "TMP", "TEMP", "GOTMPDIR", "DETENT_WORKER_SCRATCH", "PWD", "OLDPWD", "SHLVL", "_"}, key) {
			continue
		}
		if value != "" && (key == "NODE_OPTIONS" || strings.HasPrefix(key, "PYTHON") || strings.HasPrefix(strings.ToLower(key), "npm_config_")) {
			return ""
		}
		values[key] = strings.ReplaceAll(value, command.Dir, "<workspace>")
	}
	environment, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	tools, err := json.Marshal(identities)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(append(append(canonical, environment...), tools...))
	return hex.EncodeToString(digest[:])
}

func validationProbe(ctx context.Context, command *exec.Cmd, text string) *exec.Cmd {
	if command.Path == "" || len(command.Args) < 2 {
		return nil
	}
	probe := commandshell.Command(ctx, text, command.Path)
	if !slices.Equal(probe.Args[1:len(probe.Args)-1], command.Args[1:len(command.Args)-1]) {
		return nil
	}
	probe.Dir, probe.Env, probe.WaitDelay = command.Dir, slices.Clone(command.Env), workspaceCommandWaitDelay
	return probe
}

func validationExecutable(ctx context.Context, command *exec.Cmd, name string) (string, error) {
	text := "command -v '" + strings.ReplaceAll(name, "'", "'\\''") + "'"
	if len(command.Args) >= 3 && strings.EqualFold(command.Args[1], "/C") {
		if strings.ContainsAny(name, "\"%!?^&|<>()") {
			return "", fs.ErrInvalid
		}
		text = "where \"" + name + "\""
	} else if len(command.Args) >= 3 && strings.EqualFold(command.Args[len(command.Args)-2], "-Command") {
		text = "(Get-Command -Name '" + strings.ReplaceAll(name, "'", "''") + "' -CommandType Application -ErrorAction Stop).Source"
	}
	probe := validationProbe(ctx, command, text)
	if probe == nil {
		return "", fs.ErrInvalid
	}
	output, err := probe.Output()
	if err != nil {
		return "", err
	}
	path := strings.TrimSpace(string(output))
	if path == "" || strings.ContainsAny(path, "\r\n") {
		return "", fs.ErrInvalid
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(command.Dir, path)
	}
	return path, nil
}

func validationDirectoryIdentity(path string) (identity []byte, resultErr error) {
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.Close()) }()
	return validationRootIdentity(root)
}

func validationRootIdentity(root *os.Root) ([]byte, error) {
	digest := sha256.New()
	err := fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := root.Stat(path)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return json.NewEncoder(digest).Encode(struct{ Path, Mode string }{path, info.Mode().String()})
		}
		if !info.Mode().IsRegular() {
			return fs.ErrInvalid
		}
		file, err := root.Open(path)
		if err != nil {
			return err
		}
		identity := sha256.New()
		_, copyErr := io.Copy(identity, file)
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		return json.NewEncoder(digest).Encode(struct{ Path, Mode, SHA string }{path, info.Mode().String(), hex.EncodeToString(identity.Sum(nil))})
	})
	if err != nil {
		return nil, err
	}
	return digest.Sum(nil), nil
}

func validationLocalNodeSource(value any) bool {
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if key == "link" && child == true {
				return true
			}
			if validationLocalNodeSource(child) {
				return true
			}
		}
	case []any:
		for _, child := range value {
			if validationLocalNodeSource(child) {
				return true
			}
		}
	case string:
		return strings.HasPrefix(value, "file:") || strings.HasPrefix(value, "link:") || strings.HasPrefix(value, "workspace:") || strings.HasPrefix(value, "../") || strings.HasPrefix(value, "./") || filepath.IsAbs(value)
	}
	return false
}
