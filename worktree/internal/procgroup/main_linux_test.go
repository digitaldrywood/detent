package procgroup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/testenv"
)

func TestMain(m *testing.M) {
	if os.Getenv("DETENT_CAPABILITY_ZERO_HELPER") == "1" {
		if err := sandboxWithoutInheritanceHelper(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	if err := testenv.ClearGitEnvironment(); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

func sandboxWithoutInheritanceHelper() error {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return fmt.Errorf("read helper capabilities: %w", err)
	}
	inherited := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok && key == "CapInh" {
			inherited = strings.TrimSpace(value)
		}
	}
	if inherited != "0000000000000000" {
		return fmt.Errorf("helper inherited capabilities = %q, want zero", inherited)
	}
	if err := os.Setenv("PATH", os.Getenv("DETENT_CAPABILITY_EMPTY_PATH")); err != nil {
		return fmt.Errorf("set empty helper PATH: %w", err)
	}
	ctx := isolation.WithPolicy(context.Background(), isolation.Policy{Tier: isolation.Sandbox})
	cmd := exec.CommandContext(ctx, os.Getenv("DETENT_CAPABILITY_ZERO_PATH"))
	originalPath := cmd.Path
	Configure(ctx, cmd)
	if cmd.Err != nil || cmd.Path != originalPath {
		return errors.New("zero-capability launch unnecessarily requires drop utility")
	}
	if _, err := io.WriteString(os.Stdout, "ready\n"); err != nil {
		return fmt.Errorf("write helper readiness: %w", err)
	}
	if _, err := io.ReadFull(os.Stdin, make([]byte, len("exit\n"))); err != nil {
		return fmt.Errorf("wait for helper exit: %w", err)
	}
	return nil
}
