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

func CommandWithArgs(ctx context.Context, command string, shellName string, args []string) *exec.Cmd {
	return CommandWithArgsForOS(ctx, command, shellName, args, runtime.GOOS)
}

func CommandWithArgsForOS(ctx context.Context, command string, shellName string, args []string, goos string) *exec.Cmd {
	spec := CommandSpecWithArgsForOS(command, shellName, args, goos)
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

func CommandSpecWithArgsForOS(command string, shellName string, args []string, goos string) CommandSpec {
	return CommandSpecForOS(commandWithArgsForOS(command, shellName, args, goos), shellName, goos)
}

func commandWithArgsForOS(command string, shellName string, args []string, goos string) string {
	if len(args) == 0 {
		return command
	}

	quote := argQuoterForOS(shellName, goos)
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, command)
	for _, arg := range args {
		parts = append(parts, quote(arg))
	}
	return strings.Join(parts, " ")
}

func argQuoterForOS(shellName string, goos string) func(string) string {
	shellName = NormalizeForOS(shellName, goos)
	base := shellBase(shellName)
	if goos == "windows" {
		switch {
		case isPowerShell(base):
			return quotePowerShellArg
		case isPOSIXShell(base):
			return quotePOSIXArg
		default:
			return quoteCmdArg
		}
	}
	return quotePOSIXArg
}

func quotePOSIXArg(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "'\\''") + "'"
}

func quotePowerShellArg(arg string) string {
	return "'" + strings.ReplaceAll(arg, "'", "''") + "'"
}

func quoteCmdArg(arg string) string {
	arg = strings.ReplaceAll(arg, "%", "%%")
	arg = strings.ReplaceAll(arg, `"`, `\"`)
	return `"` + arg + `"`
}

func shellBase(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	base := strings.ToLower(path.Base(name))
	return strings.TrimSuffix(base, ".exe")
}

func isPowerShell(base string) bool {
	switch base {
	case "powershell", "pwsh":
		return true
	default:
		return false
	}
}

func isPOSIXShell(base string) bool {
	switch base {
	case "sh", "bash", "dash", "zsh", "ksh", "ash":
		return true
	default:
		return false
	}
}
