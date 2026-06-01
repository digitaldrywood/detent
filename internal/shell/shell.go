package shell

import (
	"context"
	"os/exec"
	"path"
	"runtime"
	"strings"
)

type CommandSpec struct {
	Name string
	Args []string
}

func Default() string {
	return DefaultForOS(runtime.GOOS)
}

func DefaultForOS(goos string) string {
	if goos == "windows" {
		return "cmd"
	}
	return "sh"
}

func Normalize(name string) string {
	return NormalizeForOS(name, runtime.GOOS)
}

func NormalizeForOS(name string, goos string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return DefaultForOS(goos)
	}
	return name
}

func Command(ctx context.Context, command string, shellName string) *exec.Cmd {
	return CommandForOS(ctx, command, shellName, runtime.GOOS)
}

func CommandForOS(ctx context.Context, command string, shellName string, goos string) *exec.Cmd {
	spec := CommandSpecForOS(command, shellName, goos)
	return exec.CommandContext(ctx, spec.Name, spec.Args...) // #nosec G204 -- workflow shell and command are operator-supplied.
}

func CommandSpecForOS(command string, shellName string, goos string) CommandSpec {
	shellName = NormalizeForOS(shellName, goos)
	if goos != "windows" {
		return CommandSpec{Name: shellName, Args: []string{"-c", command}}
	}

	base := shellBase(shellName)
	switch {
	case base == "cmd" || base == "cmd.exe":
		return CommandSpec{Name: shellName, Args: []string{"/C", command}}
	case base == "powershell" || base == "powershell.exe" || base == "pwsh" || base == "pwsh.exe":
		return CommandSpec{Name: shellName, Args: []string{"-NoLogo", "-NoProfile", "-NonInteractive", "-Command", command}}
	case isPOSIXShell(base):
		return CommandSpec{Name: shellName, Args: []string{"-c", command}}
	default:
		return CommandSpec{Name: shellName, Args: []string{"/C", command}}
	}
}

func shellBase(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	return strings.ToLower(path.Base(name))
}

func isPOSIXShell(base string) bool {
	switch base {
	case "sh", "bash", "dash", "zsh", "ksh", "ash":
		return true
	default:
		return false
	}
}
