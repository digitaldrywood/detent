package cli

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	workflowconfig "github.com/digitaldrywood/detent/internal/config"
)

func newConfigMigrateCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "migrate [FILE]",
		Short:   "Migrate workflow state lists to ordered tracker.lanes",
		Example: "detent config migrate detent.yaml\ndetent config migrate WORKFLOW.md",
		Args:    cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			path := "detent.yaml"
			if len(args) > 0 {
				path = args[0]
			}
			changed, err := workflowconfig.MigrateTrackerLanesFile(path)
			if err != nil {
				return fmt.Errorf("migrate %s: %w", path, err)
			}
			result := struct {
				Path    string `json:"path"`
				Changed bool   `json:"changed"`
			}{path, changed}
			return out.Write(func(w io.Writer) error {
				_, err := fmt.Fprintf(w, "%s: tracker.lanes migration changed=%t\n", path, changed)
				return err
			}, result)
		},
	}
}
