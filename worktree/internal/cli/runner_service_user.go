package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	servicepkg "github.com/digitaldrywood/detent/internal/service"
)

const runnerServiceSystemPath = "/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin"

func registerRunnerAsUser(cmd *cobra.Command, username, token string, args []string) error {
	account, err := user.Lookup(username)
	if err != nil {
		return fmt.Errorf("resolve dedicated runner account: %w", err)
	}
	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve runner executable: %w", err)
	}
	command, err := runnerServiceUserCommand(cmd.Context(), runtime.GOOS, account, binary, args)
	if err != nil {
		return err
	}
	command.Stdin = strings.NewReader(token)
	command.Stdout = cmd.OutOrStdout()
	command.Stderr = cmd.ErrOrStderr()
	if err := command.Run(); err != nil {
		return fmt.Errorf("register as dedicated runner account (requires noninteractive sudo, an accessible Detent binary and an active user service manager): %w", err)
	}
	return nil
}

func runnerServiceUserCommand(ctx context.Context, goos string, account *user.User, binary string, args []string) (*exec.Cmd, error) {
	if goos != "darwin" && goos != "linux" {
		return nil, servicepkg.ErrUnsupported
	}
	uid, err := strconv.ParseUint(account.Uid, 10, 32)
	if err != nil || uid == 0 || !filepath.IsAbs(account.HomeDir) || filepath.Clean(account.HomeDir) == "/" {
		return nil, errors.New("dedicated runner requires a non-root account with an absolute home directory")
	}
	path := strings.Join([]string{filepath.Join(account.HomeDir, ".local", "bin"), filepath.Join(account.HomeDir, ".npm-global", "bin"), filepath.Join(account.HomeDir, "bin"), runnerServiceSystemPath}, ":")
	if goos == "darwin" {
		path += ":/opt/homebrew/bin"
	}
	command := exec.CommandContext(ctx, "/usr/bin/sudo", "-n", "-H")
	command.Args = append(command.Args,
		"-u", "#"+strconv.FormatUint(uid, 10), "--", "/usr/bin/env", "-i",
		"HOME="+account.HomeDir, "USER="+account.Username, "LOGNAME="+account.Username, "PATH="+path)
	if goos == "linux" {
		runtimeDir := "/run/user/" + strconv.FormatUint(uid, 10)
		command.Args = append(command.Args, "XDG_RUNTIME_DIR="+runtimeDir, "DBUS_SESSION_BUS_ADDRESS=unix:path="+runtimeDir+"/bus")
	}
	command.Args = append(command.Args, binary)
	command.Args = append(command.Args, args...)
	command.Env = []string{"PATH=" + runnerServiceSystemPath}
	command.Dir = "/"
	return command, nil
}
