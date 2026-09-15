package cli

import (
	"context"
	"fmt"

	"github.com/digitaldrywood/detent/internal/workspace"
)

func checkDoctorSharedCache(ctx context.Context, projectID, root string) doctorCheck {
	check := doctorCheck{Name: "Project " + projectID + " shared cache", Status: doctorOK}
	resolved, err := expandDoctorWorkspacePath(root)
	if err != nil {
		check.Status = doctorWarn
		check.Detail = err.Error()
		return check
	}
	usage := workspace.InspectSharedCache(ctx, resolved, projectID)
	check.Detail = fmt.Sprintf("%s: total=%d bytes, go-build=%d, go-mod=%d, go-bin=%d, golangci-lint=%d; build budget=%d bytes", usage.Path, usage.TotalBytes, usage.BuildBytes, usage.ModuleBytes, usage.BinBytes, usage.LintBytes, usage.BudgetBytes)
	if usage.Error != "" {
		check.Status = doctorWarn
		check.Detail += "; " + usage.Error
	}
	if usage.BuildBytes > usage.BudgetBytes {
		check.Status = doctorWarn
		check.Hint = "The existing workspace cleanup sweep trims the shared build cache to its budget."
	}
	return check
}
