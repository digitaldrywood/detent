package cli

import (
	"encoding/json"
	"errors"
	"os/user"
 "os"
 "path/filepath"
 "slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/hubsecrets"
	"github.com/digitaldrywood/detent/internal/hubserver"
)

func newHubSecretsCommand(lookupEnv func(string) string) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "secrets",
		Short:   "Maintain encrypted Hub provider secrets",
		Example: "detent hub secrets rotate --database /var/lib/detent/hub.db",
		Args:    NoArgs,
	}
	var databasePath string
	rotate := &cobra.Command{
		Use:          "rotate",
		Short:        "Rewrap data keys in a stopped Hub using the active environment key version",
		Example:      "detent hub secrets rotate --database /var/lib/detent/hub.db",
		Args:         NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(databasePath) == "" {
				return errors.New("hub secrets rotate requires --database")
			}
			keys, err := hubsecrets.FromEnvironment(lookupEnv)
			if err != nil {
				return err
			}
			if keys == nil {
				return hubsecrets.ErrUnavailable
			}
			actor, err := user.Current()
			if err != nil {
				return errors.New("resolve operator identity for secret rotation audit")
			}
			count, err := hubserver.RotateProjectSecrets(cmd.Context(), hubserver.Config{DatabasePath: databasePath, SecretKeys: keys}, "operator:"+actor.Username)
			if err != nil {
				return err
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(struct {
				Rotated    int `json:"rotated"`
				KeyVersion int `json:"key_version"`
			}{count, keys.Version()})
		},
	}
	rotate.Flags().StringVar(&databasePath, "database", "", "stopped Hub database to rotate")
	cmd.AddCommand(rotate, newHubSecretsStatusCommand(lookupEnv))
	return cmd
}

type hubSecretsStatusError struct {
 code int
}

func (e *hubSecretsStatusError) Error() string {
 if e.code == 3 {
  return "Hub secret rotation is incomplete"
 }
 return "Hub secret preflight failed"
}

type hubSecretsStatus struct {
 ActiveVersion int `json:"active_version"`
 ConfiguredVersions []int `json:"configured_versions"`
 Databases []hubserver.SecretDatabaseStatus `json:"databases"`
 MissingVersions []int `json:"missing_versions"`
 Retirable []int `json:"retirable"`
 Problems []hubserver.SecretRowProblem `json:"problems"`
 Problem string `json:"problem,omitempty"`
}

func newHubSecretsStatusCommand(lookupEnv func(string) string) *cobra.Command {
 var databasePath, tenantRoot string
 cmd := &cobra.Command{
  Use: "status",
  Short: "Check encrypted Hub secrets with the proposed environment keyring without writing",
  Example: "detent hub secrets status --database /var/lib/detent/hub.db",
  Args: NoArgs,
  SilenceUsage: true,
  RunE: func(cmd *cobra.Command, _ []string) error {
   report := hubSecretsStatus{
    ConfiguredVersions: []int{}, Databases: []hubserver.SecretDatabaseStatus{},
    MissingVersions: []int{}, Retirable: []int{}, Problems: []hubserver.SecretRowProblem{},
   }
   code := 0
   keys, err := hubsecrets.FromEnvironment(lookupEnv)
   switch {
   case (strings.TrimSpace(databasePath) == "") == (strings.TrimSpace(tenantRoot) == ""):
    report.Problem = "exactly one of --database or --tenant-root is required"
    code = 2
   case err != nil:
    report.Problem = "invalid_keyring"
    code = 2
   default:
    report.ActiveVersion = keys.Version()
    report.ConfiguredVersions = keys.Versions()
    paths := []string{databasePath}
    if tenantRoot != "" {
     paths, err = hubSecretTenantDatabases(tenantRoot)
     if err != nil {
      report.Problem = "unreadable_tenant_root"
      code = 2
     }
    }
    referenced := make(map[int]bool)
    missing := make(map[int]bool)
    complete := err == nil
    for _, path := range paths {
     status := hubserver.SecretStatus(cmd.Context(), path, keys)
     report.Databases = append(report.Databases, status)
     if status.Problem != "" {
      complete = false
      code = 2
     }
     for _, count := range status.Counts {
      referenced[count.MasterKeyVersion] = true
     }
     for _, problem := range status.Problems {
      report.Problems = append(report.Problems, problem)
      if problem.Problem == "missing_key" {
       missing[problem.MasterKeyVersion] = true
      }
      if problem.Problem != "not_active_version" {
       code = 2
      } else if code == 0 {
       code = 3
      }
     }
    }
    for version := range missing {
     report.MissingVersions = append(report.MissingVersions, version)
    }
    slices.Sort(report.MissingVersions)
    if complete {
     for _, version := range report.ConfiguredVersions {
      if !referenced[version] {
       report.Retirable = append(report.Retirable, version)
      }
     }
    }
   }
   if err := json.NewEncoder(cmd.OutOrStdout()).Encode(report); err != nil {
    return &hubSecretsStatusError{code: 2}
   }
   if code != 0 {
    return &hubSecretsStatusError{code: code}
   }
   return nil
  },
 }
 cmd.Flags().StringVar(&databasePath, "database", "", "Hub database to inspect read-only")
 cmd.Flags().StringVar(&tenantRoot, "tenant-root", "", "scan every tenant directory's hub.db read-only")
 return cmd
}

func hubSecretTenantDatabases(root string) ([]string, error) {
 entries, err := os.ReadDir(root)
 if err != nil {
  return nil, err
 }
 paths := []string{}
 for _, entry := range entries {
  if !entry.IsDir() {
   continue
  }
  path := filepath.Join(root, entry.Name(), "hub.db")
  if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
   continue
  }
  paths = append(paths, path)
 }
 return paths, nil
}
