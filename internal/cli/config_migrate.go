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
		Short:   "Remove retired keys and migrate workflow state lists",
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
			removed, changed, err := workflowconfig.MigrateWorkflowFile(path)
			if err != nil {
				return fmt.Errorf("migrate %s: %w", path, err)
			}
			result := struct {
				Path    string   `json:"path"`
				Changed bool     `json:"changed"`
				Removed []string `json:"removed,omitempty"`
			}{path, changed, removed}
			return out.Write(func(w io.Writer) error {
				for _, key := range removed {
					if _, err := fmt.Fprintf(w, "%s: removed %s\n", path, key); err != nil {
						return err
					}
				}
				_, err := fmt.Fprintf(w, "%s: tracker.lanes migration changed=%t\n", path, changed)
				return err
			}, result)
		},
	}
}
