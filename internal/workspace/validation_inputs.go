package workspace

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/gate"
)

var landingValidationRecipe = regexp.MustCompile(`^bash scripts/check-landing\.sh "([^"\r\n]+)" "([1-9][0-9]*)" "([^"\r\n]+)"$`)
var generatedGoBuildPrefixMap = regexp.MustCompile(`(^|[ \t])-ffile-prefix-map=[^ \t=]+[/\\]go-build[0-9]+=([^ \t]+)([ \t]|$)`)

func reviewCommandEvidence(ctx context.Context, command *exec.Cmd, validation string) *gate.CommandEvidence {
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		if key == "GOFLAGS" {
			for _, flag := range strings.Fields(value) {
				if strings.HasPrefix(flag, "-exec") || strings.HasPrefix(flag, "-modfile") || strings.HasPrefix(flag, "-overlay") || strings.HasPrefix(flag, "-C") {
					return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
				}
			}
		}
	}
	if directGoValidation(validation) {
		return validationEnvironment(ctx, command)
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
	resolve := exec.CommandContext(ctx, "make", "--no-print-directory", "-n", "check-landing")
	resolve.Dir, resolve.Env, resolve.WaitDelay = command.Dir, command.Env, workspaceCommandWaitDelay
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
	module, err := os.ReadFile(filepath.Join(command.Dir, "go.mod"))
	if err != nil {
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
	probe := &exec.Cmd{Dir: command.Dir, Env: append(slices.Clone(command.Env), "GOTOOLCHAIN="+toolchain, "GOMAXPROCS=1")}
	evidence = validationEnvironment(ctx, probe)
	if !evidence.Environment.Known() || evidence.InputsSHA == "" {
		return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(evidence.InputsSHA + "\n" + strings.ReplaceAll(string(output), command.Dir, "<workspace>") + "\n" + strings.TrimSpace(base)))
	linter := args[3]
	if !filepath.IsAbs(linter) {
		linter = filepath.Join(command.Dir, linter)
	}
	paths := []string{linter}
	for _, name := range []string{"go", "make", "bash", "node", "npm", "python3"} {
		path, err := exec.LookPath(name)
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
	evidence.InputsSHA = hex.EncodeToString(digest.Sum(nil))
	return evidence
}

func validationInputs(command *exec.Cmd, observed []byte) string {
	var goEnvironment map[string]string
	if json.Unmarshal(observed, &goEnvironment) != nil {
		return ""
	}
	for key, value := range goEnvironment {
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
		values[key] = strings.ReplaceAll(value, command.Dir, "<workspace>")
	}
	for _, flag := range strings.Fields(values["GOFLAGS"]) {
		if strings.HasPrefix(flag, "-toolexec") {
			path, ok := strings.CutPrefix(flag, "-toolexec=")
			if !ok || path == "" || strings.ContainsAny(path, "\"'`$;&|<>\n\r()%!^") {
				return ""
			}
			if !filepath.IsAbs(path) {
				path, err = exec.LookPath(path)
				if err != nil {
					return ""
				}
			}
			content, err := os.ReadFile(path)
			if err != nil {
				return ""
			}
			digest := sha256.Sum256(content)
			values["toolexec_sha"] = hex.EncodeToString(digest[:])
		}
	}
	environment, err := json.Marshal(values)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(append(canonical, environment...))
	return hex.EncodeToString(digest[:])
}
