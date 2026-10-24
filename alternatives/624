package cli

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDoctorCandidateCompleteness(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name, counts, project, detail string
		want                          doctorStatus
	}{
		{"complete", `{"detent":0}`, "", "detent: 0 candidates missing", doctorOK},
		{"missing", `{"detent":3}`, "", "detent: 3 candidates missing", doctorWarn},
		{"unknown", `{"detent":null}`, "", "detent: comparison unavailable", doctorWarn},
		{"older server", `{}`, "", "comparison unavailable", doctorWarn},
		{"project scope", `{"detent":3,"other":0}`, "other", "other: 0 candidates missing", doctorOK},
	} {
		t.Run(tt.name, func(t *testing.T) {
			body := `{"status":"ok","mode":"fleet","checks":{"hub":"ok","store":"ok","registry":"ok","connector":"ok"},"candidates_missing_vs_tracker":` + tt.counts + `}`
			got := checkDoctorCandidateCompleteness(t.Context(), BootConfig{}, tt.project, doctorDeps{httpDo: func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body))}, nil
			}})
			if got.Status != tt.want || !strings.Contains(got.Detail, tt.detail) {
				t.Fatalf("check=%#v", got)
			}
		})
	}
}
