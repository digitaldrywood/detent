package shell

import (
	"context"
	"fmt"
	"os"
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
	return NormalizeForOS("", goos)
}

func Normalize(name string) string {
	return NormalizeForOS(name, runtime.GOOS)
}

func NormalizeForOS(name string, goos string) string {
	name = strings.TrimSpace(name)
	if name != "" {
		return name
	}
	if goos == "windows" {
		return "cmd"
	}
	return "sh"
}

type UnavailableError struct {
	Shell string
	Err   error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("hook_shell_unavailable: shell %q is unavailable on this runner", e.Shell)
}

func (e *UnavailableError) Unwrap() error { return e.Err }

func ResolveForOS(name string, goos string, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) (string, error) {
	name = NormalizeForOS(name, goos)
	resolved, err := lookPath(name)
	if err == nil {
		return resolved, nil
	}
	base := shellBase(name)
	if goos == "windows" && !strings.ContainsAny(name, `/\\:`) && (base == "bash" || base == "sh") {
		candidate := `C:\Program Files\Git\bin\` + base + ".exe"
		if info, statErr := stat(candidate); statErr == nil && info.Mode().IsRegular() {
			return candidate, nil
		}
	}
	return "", &UnavailableError{Shell: name, Err: err}
}

func resolveCommandSpecForOS(command string, shellName string, goos string, lookPath func(string) (string, error), stat func(string) (os.FileInfo, error)) (CommandSpec, error) {
	resolved, err := ResolveForOS(shellName, goos, lookPath, stat)
	if err != nil {
		return CommandSpec{}, err
	}
	spec := CommandSpecForOS(command, shellName, goos)
	spec.Name = resolved
	return spec, nil
}

func Command(ctx context.Context, command string, shellName string) *exec.Cmd {
	return CommandForOS(ctx, command, shellName, runtime.GOOS)
}

func CommandForOS(ctx context.Context, command string, shellName string, goos string) *exec.Cmd {
	spec, err := resolveCommandSpecForOS(command, shellName, goos, exec.LookPath, os.Stat)
	if err != nil {
		spec = CommandSpecForOS(command, shellName, goos)
	}
	cmd := exec.CommandContext(ctx, spec.Name, spec.Args...) // #nosec G204 -- workflow shell and command are operator-supplied.
	if err != nil {
		cmd.Err = err
	}
	configureCommand(cmd, goos)
	return cmd
}

func CommandWithArgs(ctx context.Context, command string, shellName string, args []string) *exec.Cmd {
	return CommandWithArgsForOS(ctx, command, shellName, args, runtime.GOOS)
}

func WithArgs(command string, shellName string, args []string) string {
	return commandWithArgsForOS(command, shellName, args, runtime.GOOS)
}

func CommandWithArgsForOS(ctx context.Context, command string, shellName string, args []string, goos string) *exec.Cmd {
	return CommandForOS(ctx, commandWithArgsForOS(command, shellName, args, goos), shellName, goos)
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
			return QuotePowerShellArg
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

func QuotePowerShellArg(arg string) string {
	var quoted strings.Builder
	quoted.WriteByte('\'')
	for _, char := range arg {
		quoted.WriteRune(char)
		switch char {
		case '\'', '‘', '’', '‚', '‛':
			quoted.WriteRune(char)
		}
	}
	quoted.WriteByte('\'')
	return quoted.String()
}

func quoteCmdArg(arg string) string {
	var quoted strings.Builder
	quoted.WriteString(`^"`)
	backslashes := 0
	for _, char := range arg {
		if char == '\\' {
			backslashes++
			continue
		}
		if char == '"' {
			quoted.WriteString(strings.Repeat(`\`, 2*backslashes+1))
		} else {
			quoted.WriteString(strings.Repeat(`\`, backslashes))
		}
		backslashes = 0
		if strings.ContainsRune("()[]%!^\"`<>&|;, *?\t", char) {
			quoted.WriteByte('^')
		}
		quoted.WriteRune(char)
	}
	quoted.WriteString(strings.Repeat(`\`, 2*backslashes))
	quoted.WriteString(`^"`)
	return quoted.String()
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
