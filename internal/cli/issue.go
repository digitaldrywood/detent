package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/digitaldrywood/detent/internal/explain"
)

func newIssueCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var explainIssue bool
	var projectID string
	cmd := &cobra.Command{
		Use:   "issue <ref>",
		Short: "Inspect an issue through the running Detent service",
		Long:  "Inspect an issue through the running Detent service. Issue explanation reads are bounded HTTP requests and never open the runtime database or contact the tracker directly.",
		Example: strings.TrimSpace(`detent issue '#1643' --explain --project detent
detent issue digitaldrywood/detent#1643 --explain --project detent --format json
detent issue https://github.com/digitaldrywood/detent/issues/1643 --explain --project detent | jq '.current_lane'`),
		Args: ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !explainIssue {
				return NewValidationError("issue operation is required", "Use --explain to read the issue explanation model.", nil)
			}
			if strings.TrimSpace(projectID) == "" {
				return NewValidationError("--project is required", "Pass the Detent project ID that owns the issue, for example --project detent.", nil)
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			client, err := newDashboardReadClient(cmd.Context(), derefString(configPath), derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), opts)
			if err != nil {
				return err
			}
			result, err := client.ExplainIssue(cmd.Context(), projectID, args[0])
			if err != nil {
				return classifyDashboardReadError(err)
			}
			return out.Write(func(writer io.Writer) error {
				return writeIssueExplanationPretty(writer, result)
			}, result)
		},
	}
	cmd.Flags().BoolVar(&explainIssue, "explain", false, "show the versioned issue explanation read model")
	cmd.Flags().StringVar(&projectID, "project", "", "Detent project ID that owns the issue (required)")
	return cmd
}

func classifyDashboardReadError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewClassifiedError(ErrDashboardTimeout, errorCodeDashboardTimeout, "dashboard API request timed out", "Confirm Detent is responsive, or retry the command.", nil)
	}
	var transport *DashboardTransportError
	if errors.As(err, &transport) {
		if transport.Timeout {
			return NewClassifiedError(ErrDashboardTimeout, errorCodeDashboardTimeout, "dashboard API request timed out", "Confirm Detent is responsive, or retry the command.", nil)
		}
		return NewClassifiedError(ErrDashboardUnreachable, errorCodeDashboardUnreachable, "dashboard service is stopped or unreachable", "Start Detent or verify the configured host and port.", nil)
	}
	var response *DashboardResponseError
	if !errors.As(err, &response) {
		return err
	}
	detail := strings.TrimSpace(response.Message)
	switch {
	case response.StatusCode == http.StatusUnauthorized:
		return NewClassifiedError(ErrDashboardUnauthorized, errorCodeDashboardUnauthorized, detail, "Set DETENT_API_TOKEN to a valid read credential or update api_token in the resolved config.", nil)
	case response.StatusCode == http.StatusForbidden:
		return NewClassifiedError(ErrDashboardForbidden, errorCodeDashboardForbidden, detail, "Use a credential with read scope for the requested project.", nil)
	case response.Code == "ambiguous_reference":
		return NewClassifiedError(ErrDashboardAmbiguousReference, errorCodeAmbiguousReference, detail, "Use an issue ID, canonical identifier, or full issue URL within the selected project.", nil)
	case response.StatusCode == http.StatusNotFound || response.Code == "issue_not_found":
		return NewClassifiedError(ErrDashboardIssueNotFound, errorCodeIssueNotFound, detail, "Verify --project and the issue reference.", nil)
	case response.Code == "version_conflict":
		return NewClassifiedError(ErrDashboardUnsupportedModel, errorCodeUnsupportedModelVersion, detail, "Upgrade Detent so the CLI and running service support the same read-model schema.", nil)
	case response.Code == "bad_request":
		return NewClassifiedError(ErrValidation, errorCodeValidation, detail, "Verify --project and the issue reference.", nil)
	case response.Code == "runtime_unavailable":
		return NewClassifiedError(ErrDashboardRuntimeUnavailable, errorCodeRuntimeUnavailable, detail, "Retry after the running Detent service publishes its issue explanation runtime.", nil)
	default:
		return NewClassifiedError(ErrDashboardRequestFailed, errorCodeDashboardRequestFailed, detail, "Inspect the running Detent service and retry the command.", nil)
	}
}

func writeIssueExplanationPretty(writer io.Writer, result explain.IssueExplanation) error {
	identity := result.Identity.Identifier
	if identity == "" {
		identity = result.Identity.IssueID
	}
	if identity == "" && result.Identity.Number > 0 {
		identity = fmt.Sprintf("#%d", result.Identity.Number)
	}
	lines := []string{
		"Issue: " + identity,
		"Project: " + result.Identity.ProjectID,
		"Lane: " + result.CurrentLane.Name,
		"Lane freshness: " + string(result.CurrentLane.Freshness),
		fmt.Sprintf("Lane degraded: %t", result.CurrentLane.Degraded),
		"Eligibility: " + string(result.Eligibility.State),
		"Required gate: " + string(result.RequiredGate.State),
		"Observed: " + result.ObservedAt.Format(time.RFC3339),
	}
	if result.Identity.Title != "" {
		lines = append(lines, "")
		copy(lines[2:], lines[1:])
		lines[1] = "Title: " + result.Identity.Title
	}
	if result.Attempt != nil {
		lines = append(lines, fmt.Sprintf("Attempt: %d (%s)", result.Attempt.ID, result.Attempt.Status))
	}
	for _, source := range result.Sources {
		value := string(source.State)
		if source.Code != "" {
			value += " (" + source.Code + ")"
		}
		lines = append(lines, "Source "+source.Name+": "+value)
	}
	_, err := fmt.Fprintln(writer, strings.Join(lines, "\n"))
	return err
}
