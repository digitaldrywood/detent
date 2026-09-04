package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

func newStateCommand(configPath *string, host *string, port *int, opts options) *cobra.Command {
	var projectID string
	cmd := &cobra.Command{
		Use:   "state",
		Short: "Read the bounded fleet telemetry state",
		Long:  "Read a bounded projection of the public telemetry state from the running Detent service. Every JSON array is limited to the first 100 entries in service order, and the truncation field reports omitted entries by JSON Pointer path.",
		Example: strings.TrimSpace(`detent state
detent state --project detent
detent state --format json | jq '.running'`),
		Args: NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if flagChanged(cmd, "project") && strings.TrimSpace(projectID) == "" {
				return NewValidationError("--project must not be blank", "Pass a configured Detent project ID or omit --project for fleet state.", nil)
			}
			out, err := OutputForCommand(cmd)
			if err != nil {
				return err
			}
			client, err := newDashboardReadClient(cmd.Context(), derefString(configPath), derefString(host), derefInt(port, -1), flagChanged(cmd, "port"), opts)
			if err != nil {
				return err
			}
			result, err := client.State(cmd.Context(), projectID)
			if err != nil {
				return classifyStateReadError(err)
			}
			return out.Write(func(writer io.Writer) error {
				return writeStatePretty(writer, result)
			}, result)
		},
	}
	cmd.Flags().StringVar(&projectID, "project", "", "limit state to one Detent project ID")
	return cmd
}

func classifyStateReadError(err error) error {
	var response *DashboardResponseError
	if errors.As(err, &response) && response.StatusCode == http.StatusNotFound && response.Code == "project_not_found" {
		return NewClassifiedError(ErrProjectNotFound, errorCodeProjectNotFound, response.Message, "Verify --project matches a configured Detent project ID.", nil)
	}
	return classifyDashboardReadError(err)
}

func writeStatePretty(writer io.Writer, state DashboardState) error {
	status := stateString(state.field("status"))
	refreshStatus := stateNestedString(state.field("refresh"), "status")
	errorCode := stateNestedString(state.field("error"), "code")
	if status == "" && errorCode != "" {
		status = "degraded"
	}
	if status == "" {
		status = "unknown"
	}
	degraded := refreshStatus == "degraded" || errorCode != "" || stateRefreshSourceDegraded(state.field("refresh"))

	lines := []string{
		"Status: " + status,
		"Generated at: " + stateString(state.field("generated_at")),
		fmt.Sprintf("Degraded: %t", degraded),
	}
	if refreshStatus != "" {
		lines = append(lines, "Refresh status: "+refreshStatus)
	}
	if refreshedAt := stateNestedString(state.field("refresh"), "last_refresh_at"); refreshedAt != "" {
		lines = append(lines, "Last refresh: "+refreshedAt)
	}
	if staleAfter := stateNestedString(state.field("refresh"), "stale_after_seconds"); staleAfter != "" {
		lines = append(lines, "Stale after seconds: "+staleAfter)
	}
	if errorCode != "" {
		lines = append(lines, "Snapshot error: "+errorCode+": "+stateNestedString(state.field("error"), "message"))
	}
	if counts, ok := state.field("counts").(map[string]any); ok {
		lines = append(lines,
			"Running: "+stateString(counts["running"]),
			"Retrying: "+stateString(counts["retrying"]),
			"Ready: "+stateString(counts["ready"]),
			"Waiting: "+stateString(counts["waiting"]),
			"Blocked: "+stateString(counts["blocked"]),
		)
	}
	for _, pressure := range []struct {
		label        string
		field        string
		row          string
		average      string
		thresholdKey string
	}{
		{label: "Memory", field: "memory_pressure", row: "some", average: "avg60", thresholdKey: "some_avg60_max"},
		{label: "I/O", field: "io_pressure", row: "full", average: "avg10", thresholdKey: "full_avg10_max"},
		{label: "CPU", field: "cpu_pressure", row: "some", average: "avg10", thresholdKey: "some_avg10_max"},
	} {
		if line := statePressureLine(state.field(pressure.field), pressure.label, pressure.row, pressure.average, pressure.thresholdKey); line != "" {
			lines = append(lines, line)
		}
	}
	lines = append(lines, fmt.Sprintf("Truncated: %t", state.Truncation.Truncated))
	for _, collection := range state.Truncation.Collections {
		lines = append(lines, fmt.Sprintf("Truncated %s: %d omitted", collection.Path, collection.Omitted))
	}
	_, err := fmt.Fprintln(writer, strings.Join(lines, "\n"))
	return err
}

func statePressureLine(value any, label string, row string, average string, thresholdKey string) string {
	pressure, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	threshold := stateString(pressure[thresholdKey])
	if threshold == "" || threshold == "0" {
		return ""
	}
	if lastError := stateString(pressure["last_error"]); lastError != "" {
		return label + " PSI: unavailable (" + lastError + ")"
	}
	if !stateBool(pressure["supported"]) {
		return label + " PSI: unsupported"
	}
	averages := stateObject(pressure[row])
	current := stateString(averages[average])
	if current == "" {
		current = "0"
	}
	status := "admitting"
	if stateBool(pressure["dispatch_held"]) {
		status = "holding dispatch"
	}
	return fmt.Sprintf("%s PSI %s %s: %s%% / %s%% threshold (%s)", label, row, average, current, threshold, status)
}

func stateBool(value any) bool {
	typed, ok := value.(bool)
	return ok && typed
}

func stateObject(value any) map[string]any {
	typed, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return typed
}

func stateNestedString(value any, key string) string {
	object, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	return stateString(object[key])
}

func stateString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case bool:
		return strconv.FormatBool(typed)
	default:
		return ""
	}
}

func stateRefreshSourceDegraded(value any) bool {
	refresh, ok := value.(map[string]any)
	if !ok {
		return false
	}
	sources, ok := refresh["sources"].([]any)
	if !ok {
		return false
	}
	for _, source := range sources {
		object, ok := source.(map[string]any)
		if ok && object["degraded"] == true {
			return true
		}
	}
	return false
}
