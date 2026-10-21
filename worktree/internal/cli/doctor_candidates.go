package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func checkDoctorCandidateCompleteness(ctx context.Context, boot BootConfig, projectID string, deps doctorDeps) doctorCheck {
	check := doctorCheck{Name: "Candidate completeness", Status: doctorOK}
	probe, err := probeDoctorHealth(ctx, doctorLiveBoot(boot, &boot.Global), deps)
	if err != nil {
		check.Status = doctorWarn
		check.Detail = "candidate comparison unavailable: " + err.Error()
		return check
	}
	var details []string
	for id, count := range probe.Health.CandidatesMissingVsTracker {
		if projectID != "" && id != projectID {
			continue
		}
		if count == nil {
			check.Status = doctorWarn
			details = append(details, id+": comparison unavailable")
			continue
		}
		details = append(details, fmt.Sprintf("%s: %d candidates missing vs tracker", id, *count))
		if *count > 0 {
			check.Status = doctorWarn
		}
	}
	if len(details) == 0 {
		check.Status = doctorWarn
		check.Detail = "candidate comparison unavailable"
		return check
	}
	sort.Strings(details)
	check.Detail = strings.Join(details, "; ") + " (last successful refresh)"
	return check
}
