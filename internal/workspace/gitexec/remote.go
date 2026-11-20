package gitexec

import (
	"context"
	"os/exec"
	"strings"
)

func RemoteArgs(args ...string) []string {
	if _, err := exec.LookPath("gh"); err != nil {
		return args
	}
	return append([]string{"-c", "credential.https://github.com.helper=!gh auth git-credential"}, args...)
}

func RemoteBranchHead(ctx context.Context, workspacePath string, remote string, branch string) (string, bool, error) {
	ref := "refs/heads/" + branch
	output, err := Run(ctx, workspacePath, RemoteArgs("ls-remote", remote, ref)...)
	if err != nil {
		return "", false, err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != ref {
			continue
		}
		return fields[0], true, nil
	}
	return "", false, nil
}
