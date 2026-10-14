package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/hubclient"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func newHubRunnerCommand(version string, lookupEnv func(string) string) *cobra.Command {
	var path, hubURL, organization, enrollmentEnv, displayName, hostIdentityPath string
	var capacity int
	command := &cobra.Command{Use: "runner", Short: "Manage a private customer-host runner identity", Example: "detent hub runner init --hub-url https://hub.example.com", Args: NoArgs}
	command.PersistentFlags().StringVar(&path, "identity-file", "", "absolute private credential file (default: user config directory/detent/runner/identity.json)")
	resolvePath := func() (string, error) {
		if path != "" {
			return path, nil
		}
		root, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(root, "detent", "runner", "identity.json"), nil
	}
	write := func(cmd *cobra.Command, identity runnerauth.Identity) error {
		output, err := OutputForCommand(cmd)
		if err != nil {
			return err
		}
		return output.Write(func(w io.Writer) error {
			_, err := fmt.Fprintf(w, "Runner: %s\nMachine: %s\n", identity.RunnerID, identity.MachineID)
			return err
		}, identity)
	}
	initialize := &cobra.Command{Use: "init", Short: "Generate and persist a host identity before requesting enrollment", Example: "detent hub runner init --hub-url https://hub.example.com", Args: NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := resolvePath()
		if err != nil {
			return err
		}
		if err := runnerPrivateLocation(path); err != nil {
			return err
		}
		var file runnerauth.File
		if hostIdentityPath != "" {
			if err := runnerPrivateLocation(hostIdentityPath); err != nil {
				return err
			}
			file, err = runnerauth.InitializeOnHost(path, hubURL, hostIdentityPath)
		} else {
			file, err = runnerauth.Initialize(path, hubURL)
		}
		if err != nil {
			return err
		}
		return write(cmd, file.Identity)
	}}
	initialize.Flags().StringVar(&hubURL, "hub-url", "", "HTTPS Hub URL (loopback HTTP is allowed)")
	initialize.Flags().StringVar(&hostIdentityPath, "host-identity-file", "", "existing enrolled identity on this host; create a distinct runner sharing its machine ID")
	enroll := &cobra.Command{Use: "enroll", Short: "Redeem one enrollment token for this host identity", Example: "detent hub runner enroll --organization org_example --display-name 'Build host'", Args: NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := resolvePath()
		if err != nil {
			return err
		}
		if !validEnvName(enrollmentEnv) {
			return errors.New("enrollment token environment variable name is invalid")
		}
		hostname, err := os.Hostname()
		if err != nil {
			return errors.New("host name is unavailable")
		}
		identity, err := hubclient.EnrollRunner(cmd.Context(), path, tracker.OrganizationID(organization), strings.TrimSpace(lookupEnv(enrollmentEnv)), hubclient.Machine{Hostname: hostname, DisplayName: displayName, Capacity: capacity, Version: firstNonBlankString(version, "dev")})
		if err != nil {
			return err
		}
		return write(cmd, identity)
	}}
	enroll.Flags().StringVar(&organization, "organization", "", "intended organization ID")
	enroll.Flags().StringVar(&enrollmentEnv, "enrollment-token-env", "DETENT_RUNNER_ENROLLMENT_TOKEN", "environment variable containing the one-time enrollment token")
	enroll.Flags().StringVar(&displayName, "display-name", "", "mutable runner display name")
	enroll.Flags().IntVar(&capacity, "capacity", 1, "permitted local concurrency")
	command.AddCommand(initialize, enroll)
	for _, action := range []string{"renew", "rotate"} {
		command.AddCommand(&cobra.Command{Use: action, Short: "Refresh the host credential using its current authority", Example: "detent hub runner " + action, Args: NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := resolvePath()
			if err != nil {
				return err
			}
			identity, err := hubclient.RefreshRunner(cmd.Context(), path, action == "rotate")
			if err != nil {
				return err
			}
			return write(cmd, identity)
		}})
	}
	return command
}

func runnerPrivateLocation(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("runner identity requires an absolute private path")
	}
	for parent := filepath.Dir(path); ; parent = filepath.Dir(parent) {
		if _, err := os.Lstat(filepath.Join(parent, ".git")); err == nil {
			return errors.New("runner identity must be outside repositories and ordinary workspaces")
		}
		if parent == filepath.Dir(parent) {
			return nil
		}
	}
}
