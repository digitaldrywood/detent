package workspace

import (
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

	"github.com/digitaldrywood/detent/internal/gate"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

var generatedGoBuildPrefixMap = regexp.MustCompile(`(^|[ \t])-ffile-prefix-map=[^ \t=]+[/\\]go-build[0-9]+=([^ \t]+)([ \t]|$)`)

// Only a tool version query has no package execution inputs. Build, vet, tests
// and landing recipes can consume external dependencies, files or services that
// this collector cannot bind. Their successful receipts remain observations,
// and each independent review must execute its configured command.
func reviewCommandEvidence(ctx context.Context, command *exec.Cmd, validation string) *gate.CommandEvidence {
	if !directGoValidation(validation) {
		return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	}
	return validationEnvironment(ctx, command)
}

func validationInputs(ctx context.Context, command *exec.Cmd, observed []byte) string {
	var goEnvironment map[string]string
	if json.Unmarshal(observed, &goEnvironment) != nil {
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
		if entry.Type()&os.ModeSymlink != 0 {
			link, err := root.Readlink(path)
			if err != nil {
				return err
			}
			if err := json.NewEncoder(digest).Encode(struct{ Path, Link string }{path, link}); err != nil {
				return err
			}
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
