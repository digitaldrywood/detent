package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/telemetry"
)

type doctorBackendCapacityDiagnostic struct {
	ProjectID       string     `json:"project_id,omitempty"`
	BackendID       string     `json:"backend_id"`
	BackendKind     string     `json:"backend_kind,omitempty"`
	Provider        string     `json:"provider,omitempty"`
	DetectedAt      time.Time  `json:"detected_at"`
	LastObservedAt  time.Time  `json:"last_observed_at"`
	ResetAt         *time.Time `json:"reset_at,omitempty"`
	ResumeAt        time.Time  `json:"resume_at"`
	NextProbeAt     *time.Time `json:"next_probe_at,omitempty"`
	LastProbeAt     *time.Time `json:"last_probe_at,omitempty"`
	LastProbeResult string     `json:"last_probe_result,omitempty"`
	LastProbeDetail string     `json:"last_probe_detail,omitempty"`
	ProbeAttempts   int        `json:"probe_attempts,omitempty"`
	Active          bool       `json:"active"`
	Enforced        bool       `json:"enforced"`
}

func checkDoctorBackendCapacity(ctx context.Context, boot BootConfig, projectID string, deps doctorDeps) doctorCheck {
	const name = "Backend capacity"
	deps = deps.withDefaults()
	outages, available := readDoctorBackendCapacityLive(ctx, boot, projectID, deps)
	if !available {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "live orchestrator state unavailable"}
	}
	diagnostics := make([]doctorBackendCapacityDiagnostic, 0, len(outages))
	for _, outage := range outages {
		diagnostics = append(diagnostics, doctorBackendCapacityDiagnostic{
			ProjectID:       outage.ProjectID,
			BackendID:       outage.BackendID,
			BackendKind:     outage.BackendKind,
			Provider:        outage.Provider,
			DetectedAt:      outage.DetectedAt,
			LastObservedAt:  outage.LastObservedAt,
			ResetAt:         outage.ResetAt,
			ResumeAt:        outage.ResumeAt,
			NextProbeAt:     outage.NextProbeAt,
			LastProbeAt:     outage.LastProbeAt,
			LastProbeResult: outage.LastProbeResult,
			LastProbeDetail: outage.LastProbeDetail,
			ProbeAttempts:   outage.ProbeAttempts,
			Active:          true,
			Enforced:        true,
		})
	}
	if len(diagnostics) == 0 {
		return doctorCheck{Name: name, Status: doctorOK, Detail: "live orchestrator reports no enforced outage", BackendCapacity: diagnostics}
	}
	latest := diagnostics[0]
	probe := "last probe: none"
	if latest.LastProbeAt != nil {
		probe = "last probe: " + latest.LastProbeAt.UTC().Format(time.RFC3339) + " (" + strings.ReplaceAll(latest.LastProbeResult, "_", " ") + ")"
	}
	return doctorCheck{
		Name:            name,
		Status:          doctorWarn,
		Detail:          fmt.Sprintf("%d enforced provider capacity outage(s); latest recorded window %s to provider-recorded %s; %s", len(diagnostics), latest.DetectedAt.UTC().Format(time.RFC3339), latest.ResumeAt.UTC().Format(time.RFC3339), probe),
		Hint:            "Detent is pausing the affected backend while bounded early canaries and live status checks probe for recovery.",
		BackendCapacity: diagnostics,
	}
}

func readDoctorBackendCapacityLive(
	ctx context.Context,
	boot BootConfig,
	projectID string,
	deps doctorDeps,
) ([]telemetry.BackendOutage, bool) {
	port := defaultWebPort
	if boot.Port != nil {
		port = *boot.Port
	}
	if port <= 0 || deps.httpDo == nil {
		return nil, false
	}
	host := unbracketIPv6Host(strings.TrimSpace(boot.Host))
	switch host {
	case "", "0.0.0.0", "::":
		host = defaultWebHost
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, strconv.Itoa(port))+"/health", nil)
	if err != nil {
		return nil, false
	}
	response, err := deps.httpDo(request)
	if err != nil {
		return nil, false
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return nil, false
	}
	var payload struct {
		BackendOutages []telemetry.BackendOutage `json:"backend_outages"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		return nil, false
	}
	filtered := make([]telemetry.BackendOutage, 0, len(payload.BackendOutages))
	for _, outage := range payload.BackendOutages {
		if strings.TrimSpace(projectID) == "" || strings.EqualFold(strings.TrimSpace(projectID), strings.TrimSpace(outage.ProjectID)) {
			filtered = append(filtered, outage)
		}
	}
	return filtered, true
}
