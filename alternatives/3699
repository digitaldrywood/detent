package workspace

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent"
	"github.com/digitaldrywood/detent/internal/gate"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
)

var landingValidationRecipe = regexp.MustCompile(`^bash scripts/check-landing\.sh "([^"\r\n]+)" "([1-9][0-9]*)" "([^"\r\n]+)"$`)

// Local shell commands are not hermetic: startup files, loaders, tools and
// runtime dependencies can change outside the checkout. Environment metadata
// remains observational; it must never authorize receipt reuse.
func reviewCommandEvidence(ctx context.Context, command *exec.Cmd, validation string) *gate.CommandEvidence {
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
	_, err = runGitAtWithEnv(ctx, command.Dir, command.Env, "rev-parse", "--verify", args[1]+"^{commit}")
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
	probe := *command
	probe.Env = append(slices.Clone(command.Env), "GOTOOLCHAIN="+toolchain, "GOMAXPROCS=1")
	evidence = validationEnvironment(ctx, &probe)
	return evidence
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
