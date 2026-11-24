package validationcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/gobudget"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

var landingValidationRecipe = regexp.MustCompile(`^bash scripts/check-landing\.sh "([^"\r\n]+)" "([1-9][0-9]*)" "([^"\r\n]+)"$`)

type Inputs struct {
	Dir  string
	Path string
	Args []string
	Env  []string
}

// Local shell commands are not hermetic: startup files, loaders, tools and
// runtime dependencies can change outside the checkout. Environment metadata
// remains observational; it must never authorize receipt reuse.
func (command Inputs) Evidence(ctx context.Context, validation string) *gate.CommandEvidence {
	if validation == "go version" {
		return command.Environment(ctx)
	}
	evidence := &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	if validation != "make check-landing" {
		return evidence
	}
	if !landingRecipeInputsMatch(command.Dir) {
		return evidence
	}
	for _, entry := range command.Env {
		key, value, _ := strings.Cut(entry, "=")
		if value != "" && slices.Contains([]string{"MAKEFILES", "MAKEFLAGS", "GNUMAKEFLAGS", "MFLAGS", "BASH_ENV", "ENV"}, key) {
			return evidence
		}
	}
	output, err := command.probe(ctx, "make --no-print-directory -n check-landing")
	if err != nil {
		return evidence
	}
	args := landingValidationRecipe.FindStringSubmatch(strings.TrimSpace(string(output)))
	if len(args) != 4 {
		return evidence
	}
	_, err = gitexec.WithEnv(ctx, command.Dir, command.Env, "rev-parse", "--verify", args[1]+"^{commit}")
	if err != nil {
		return evidence
	}
	module, moduleErr := os.ReadFile(filepath.Join(command.Dir, "go.mod"))
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
	command.Env = append(slices.Clone(command.Env), "GOTOOLCHAIN="+toolchain, "GOMAXPROCS=1")
	return command.Environment(ctx)
}

func landingRecipeInputsMatch(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "GNUmakefile" || entry.Name() == "makefile" {
			return false
		}
	}
	for _, name := range []string{"Makefile", "scripts/check-landing.sh", "scripts/check-evidence.sh", "scripts/check-landing.py", "scripts/check-landing-lint.mk", "scripts/build-custom-linter.sh"} {
		actual, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return false
		}
		supported, err := fs.ReadFile(detent.ValidationFS(), name)
		if err != nil || !bytes.Equal(actual, supported) {
			return false
		}
	}
	return true
}

func (command Inputs) probe(ctx context.Context, text string) ([]byte, error) {
	if command.Path == "" || len(command.Args) < 2 {
		return nil, errors.New("missing validation shell")
	}
	probe := commandshell.Command(ctx, text, command.Path)
	if len(probe.Args) < 2 || !slices.Equal(probe.Args[1:len(probe.Args)-1], command.Args[1:len(command.Args)-1]) {
		return nil, errors.New("validation shell arguments differ")
	}
	probe.Dir, probe.Env, probe.WaitDelay = command.Dir, slices.Clone(command.Env), gitexec.WaitDelay
	return commandshell.CommandOutput(ctx, probe, false)
}

func (command Inputs) Environment(ctx context.Context) *gate.CommandEvidence {
	evidence := &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	var observed struct {
		OS           string `json:"GOHOSTOS"`
		Architecture string `json:"GOHOSTARCH"`
		Version      string `json:"GOVERSION"`
	}
	output, err := command.probe(ctx, "go env -json")
	if err == nil && json.Unmarshal(output, &observed) == nil {
		environment := gate.CheckEnvironment{OS: observed.OS, Architecture: observed.Architecture, GoVersion: observed.Version}
		if environment.Valid() {
			evidence.Environment = environment
		}
	}
	return evidence
}

func GoBudget(ctx context.Context, environment []string) (map[string]string, error) {
	inherited := make(map[string]string)
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			inherited[key] = value
		}
	}
	variables, err := gobudget.FromContext(ctx).Environment(inherited["GOFLAGS"])
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"GOMAXPROCS", gobudget.TestProcsEnvironment} {
		if _, explicit := inherited[key]; explicit {
			delete(variables, key)
		}
	}
	return variables, nil
}
