package workspace

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

var ErrInvalidVerification = errors.New("invalid source proof")

func invalidVerification(message string) error {
	return fmt.Errorf("%w: %s", ErrInvalidVerification, message)
}

func verificationCommandError(ctx context.Context, err error, message string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ProcessState != nil && exit.ProcessState.Exited() {
		stderr := strings.ToLower(string(exit.Stderr))
		for _, diagnostic := range []string{
			"pack is corrupted", "unknown revision or path not in the working tree", "unknown revision or path not in working tree", "expected oid, not a ref", "pack checksum mismatch", "pack signature mismatch", "pack version",
			"early eof", "pack has bad object", "inflate returned",
			"invalid object", "bad object", "not a valid object name",
			"could not get object info", "unable to read tree",
			"did not receive expected object", "unresolved delta",
			"fsck error", "missing blob", "missing tree", "missing commit",
		} {
			if strings.Contains(stderr, diagnostic) {
				return invalidVerification(message)
			}
		}
		if exit.ExitCode() == 1 && strings.Contains(message, "not incorporated") {
			return invalidVerification(message)
		}
	}
	return fmt.Errorf("run source verifier: %w", err)
}

func runVerificationCommand(_ context.Context, command *exec.Cmd) ([]byte, error) {
	return command.Output()
}
