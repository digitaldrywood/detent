package procgroup

import (
	"context"
	"fmt"
	"os/exec"

	"github.com/digitaldrywood/detent/internal/isolation"
	"golang.org/x/sys/unix"
)

func configureSandboxCapabilities(ctx context.Context, cmd *exec.Cmd) error {
	policy, ok := isolation.FromContext(ctx)
	if !ok || policy.Tier != isolation.Sandbox {
		return nil
	}
	requested := len(cmd.SysProcAttr.AmbientCaps) > 0
	cmd.SysProcAttr.AmbientCaps = nil
	capabilities := [2]unix.CapUserData{}
	header := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	if err := unix.Capget(&header, &capabilities[0]); err != nil {
		return fmt.Errorf("read sandbox child capabilities: %w", err)
	}
	if !requested && capabilities[0].Inheritable == 0 && capabilities[1].Inheritable == 0 {
		return nil
	}
	path, err := exec.LookPath("setpriv")
	if err != nil {
		return fmt.Errorf("drop sandbox child capabilities: %w", err)
	}
	if cmd.Path == path && len(cmd.Args) > 3 && cmd.Args[1] == "--inh-caps=-all" && cmd.Args[2] == "--ambient-caps=-all" && cmd.Args[3] == "--" {
		return nil
	}
	args := []string{path, "--inh-caps=-all", "--ambient-caps=-all", "--", cmd.Path}
	if len(cmd.Args) > 0 {
		args = append(args, cmd.Args[1:]...)
	}
	cmd.Path, cmd.Args = path, args
	return nil
}
