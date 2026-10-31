package cli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"reflect"
	"slices"
	"strings"
	"testing"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func TestProbeDoctorLandingCredential(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, remote, diagnostic string
		remoteErr, pushErr       error
		wantErr, wantPush, https bool
	}{
		{name: "HTTPS host login", remote: "https://github.com/example-org/example-project.git", wantPush: true, https: true},
		{name: "SSH host key", remote: "git@github.com:example-org/example-project.git", wantPush: true},
		{name: "SSH URL", remote: "ssh://git@github.com/example-org/example-project.git", wantPush: true},
		{name: "missing HTTPS permission", remote: "https://github.com/example-org/example-project.git", pushErr: errors.New("exit status 128"), diagnostic: "fatal: Authentication failed", wantErr: true, wantPush: true, https: true},
		{name: "missing SSH key", remote: "git@github.com:example-org/example-project.git", pushErr: errors.New("exit status 128"), diagnostic: "Permission denied (publickey)", wantErr: true, wantPush: true},
		{name: "missing origin", remoteErr: errors.New("exit status 2"), wantErr: true},
		{name: "unsupported origin", remote: "file:///repository", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			err := probeDoctorLandingCredential(t.Context(), "/configured/checkout", func(cmd *exec.Cmd) error {
				calls++
				if !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") || !slices.Contains(cmd.Env, "GIT_ASKPASS=") || !slices.Contains(cmd.Env, "GCM_INTERACTIVE=never") || !slices.ContainsFunc(cmd.Env, func(entry string) bool {
					return strings.HasPrefix(entry, "GIT_SSH_COMMAND=") && strings.HasSuffix(entry, " -o BatchMode=yes")
				}) {
					t.Fatalf("probe permits interactive authentication")
				}
				want := []string{"git", "-C", "/configured/checkout"}
				if calls == 1 {
					want = append(want, "remote", "get-url", "--push", "origin")
					if !reflect.DeepEqual(cmd.Args, want) {
						t.Fatalf("remote command = %q, want %q", cmd.Args, want)
					}
					_, err := fmt.Fprintln(cmd.Stdout, test.remote)
					return errors.Join(test.remoteErr, err)
				}
				if test.https {
					want = append(want, "-c", "credential.helper=", "-c", "credential.helper=!gh auth git-credential")
				}
				want = append(want, "push", "--dry-run", "--no-verify", "--", "origin", "HEAD:refs/heads/detent/credential-check")
				if !reflect.DeepEqual(cmd.Args, want) {
					t.Fatalf("push command = %q, want %q", cmd.Args, want)
				}
				_, err := fmt.Fprintln(cmd.Stderr, test.diagnostic)
				return errors.Join(test.pushErr, err)
			})
			if (err != nil) != test.wantErr || (calls == 2) != test.wantPush {
				t.Fatalf("probe error = %v, calls = %d", err, calls)
			}
			if test.diagnostic != "" && (err == nil || !strings.Contains(err.Error(), test.diagnostic)) {
				t.Fatalf("probe lost diagnostic: %v", err)
			}
		})
	}
}

func TestDoctorLandingCredentialScope(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, tracker, workspace string
		pullRequest, omitted     bool
		probeErr                 error
	}{
		{name: "native Git", tracker: workflowconfig.TrackerHubNative},
		{name: "failed native credential", tracker: workflowconfig.TrackerHubNative, probeErr: errors.New("fatal: authentication failed token=private-secret")},
		{name: "PR publication", tracker: workflowconfig.TrackerHubNative, pullRequest: true, omitted: true},
		{name: "other tracker", tracker: workflowconfig.TrackerGitHub, omitted: true},
		{name: "filesystem", tracker: workflowconfig.TrackerHubNative, workspace: "filesystem", omitted: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			cfg := validDoctorWorkflow("/configured/checkout")
			cfg.Tracker.Kind, cfg.Workspace.Kind, cfg.Deliverable.GitHubPullRequest = test.tracker, test.workspace, test.pullRequest
			called := false
			deps := doctorDeps{gitLandingCredential: func(_ context.Context, source string) error {
				called = true
				if source != "/configured/checkout" {
					t.Fatalf("probe source = %q", source)
				}
				return test.probeErr
			}}
			check, ok := checkDoctorLandingCredential(t.Context(), "example-project", "/configured/checkout", cfg, deps)
			if ok == test.omitted || called == test.omitted {
				t.Fatalf("check included = %v, probe called = %v", ok, called)
			}
			if !ok {
				return
			}
			if check.Name != "Project example-project landing push credential" || (check.Status == doctorFail) != (test.probeErr != nil) {
				t.Fatalf("check = %#v", check)
			}
			if test.probeErr != nil && (!strings.Contains(check.Detail, "authentication failed") || strings.Contains(check.Detail, "private-secret") || !strings.Contains(check.Hint, "repository write access")) {
				t.Fatalf("unsafe or unhelpful failure check = %#v", check)
			}
		})
	}
}
