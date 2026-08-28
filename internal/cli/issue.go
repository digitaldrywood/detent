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
	"github.com/digitaldrywood/detent/internal/store"
)

func newIssueCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var explainIssue bool
	var acknowledgeParks bool
	var creditProgress bool
	var projectID string
	cmd := &cobra.Command{
		Use:   "issue <ref>",
		Short: "Inspect an issue through the running Detent service",
		Long:  "Inspect, acknowledge parks, or credit accepted progress through the running Detent service. Issue operations use bounded HTTP requests and never open the runtime database or contact the tracker directly.",
		Example: strings.TrimSpace(`detent issue '#1643' --explain --project detent
detent issue digitaldrywood/detent#1643 --explain --project detent --format json
	detent issue https://github.com/digitaldrywood/detent/issues/1643 --explain --project detent | jq '.current_lane'
	detent issue '#1643' --acknowledge-parks --project detent
	detent issue '#1643' --credit-progress --project detent`),
		Args: ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if issueOperationCount(explainIssue, acknowledgeParks, creditProgress) != 1 {
				return NewValidationError("exactly one issue operation is required", "Use --explain, --acknowledge-parks, or --credit-progress.", nil)
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
			if creditProgress {
				credit, err := client.CreditIssueProgress(cmd.Context(), projectID, args[0])
				if err != nil {
					return classifyDashboardReadError(err)
				}
				return out.Write(func(writer io.Writer) error {
					return writeIssueProgressCreditPretty(writer, credit)
				}, credit)
			}
			var result explain.IssueExplanation
			if acknowledgeParks {
				result, err = client.AcknowledgeIssueParks(cmd.Context(), projectID, args[0])
			} else {
				result, err = client.ExplainIssue(cmd.Context(), projectID, args[0])
			}
			if err != nil {
				return classifyDashboardReadError(err)
			}
			return out.Write(func(writer io.Writer) error {
				return writeIssueExplanationPretty(writer, result)
			}, result)
		},
	}
	cmd.Flags().BoolVar(&explainIssue, "explain", false, "show the versioned issue explanation read model")
	cmd.Flags().BoolVar(&acknowledgeParks, "acknowledge-parks", false, "acknowledge the issue's current park sequence")
	cmd.Flags().BoolVar(&creditProgress, "credit-progress", false, "reset spend-since-progress counters for this issue")
	cmd.Flags().StringVar(&projectID, "project", "", "Detent project ID that owns the issue (required)")
	return cmd
}

func issueOperationCount(operations ...bool) int {
	count := 0
	for _, enabled := range operations {
		if enabled {
			count++
		}
	}
	return count
}

func writeIssueProgressCreditPretty(writer io.Writer, credit store.IssueProgressCredit) error {
	identity := credit.Identifier
	if identity == "" {
		identity = credit.IssueID
	}
	if identity == "" {
		identity = credit.IssueURL
	}
	_, err := fmt.Fprintf(writer, "Credited accepted progress for %s at %s\n", identity, credit.CreditedAt.UTC().Format(time.RFC3339))
	return err
}

func classifyDashboardReadError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) {
		return err
	}
	var transport *DashboardTransportError
	if errors.As(err, &transport) {
		if transport.Timeout {
			return NewClassifiedError(ErrDashboardTimeout, errorCodeDashboardTimeout, transport.Error(), "Confirm Detent is responsive, or retry the command.", nil)
		}
		return NewClassifiedError(ErrDashboardUnreachable, errorCodeDashboardUnreachable, transport.Error(), "Start Detent or verify the configured host and port.", nil)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return NewClassifiedError(ErrDashboardTimeout, errorCodeDashboardTimeout, "dashboard API request timed out", "Confirm Detent is responsive, or retry the command.", nil)
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
	case response.Code == "issue_not_found":
		return NewClassifiedError(ErrDashboardIssueNotFound, errorCodeIssueNotFound, detail, "Verify --project and the issue reference.", nil)
	case response.StatusCode == http.StatusNotFound:
		return NewClassifiedError(ErrDashboardUnsupportedModel, errorCodeUnsupportedModelVersion, "running service does not support the issue explanation API", "Upgrade or restart Detent so the running service provides the issue explanation endpoint.", nil)
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
	lines = append(lines,
		fmt.Sprintf("Lifetime attempts: %d", result.ParkSummary.AttemptCount),
		fmt.Sprintf("Lifetime parks: %d", result.ParkSummary.ParkCount),
		fmt.Sprintf("Lifetime tokens: input %d, cached input %d, output %d, reasoning %d", result.ParkSummary.Tokens.InputTokens, result.ParkSummary.Tokens.CachedInputTokens, result.ParkSummary.Tokens.OutputTokens, result.ParkSummary.Tokens.ReasoningOutputTokens),
	)
	for _, cause := range result.ParkSummary.Causes {
		lines = append(lines, fmt.Sprintf("Park cause %s: %d (first %s, last %s)", cause.Cause, cause.Count, cause.FirstAt.Format(time.RFC3339), cause.LastAt.Format(time.RFC3339)))
	}
	if result.ParkSummary.AcknowledgedParkSequence > 0 {
		lines = append(lines, fmt.Sprintf("Acknowledged park sequence: %d", result.ParkSummary.AcknowledgedParkSequence))
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
