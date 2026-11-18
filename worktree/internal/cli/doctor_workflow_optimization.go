package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"
)

func confirmDoctorWorkflowOptimizationWrite(cmd *cobra.Command, report doctorWorkflowOptimizationReport) error {
	patches := doctorWorkflowOptimizationPatchesByWorkflow(report)
	if len(patches) == 0 {
		return nil
	}
	total := 0
	for _, patchSet := range patches {
		total += len(patchSet)
	}
	if cmd == nil {
		return errors.New("workflow optimization write confirmation requires a command")
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "Apply %d workflow optimization patch(es) to %d WORKFLOW.md file(s)? Type yes to continue: ", total, len(patches)); err != nil {
		return err
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if strings.ToLower(strings.TrimSpace(answer)) != "yes" {
		return errors.New("workflow optimization write cancelled")
	}
	return nil
}
