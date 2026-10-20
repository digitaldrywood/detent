package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestRetainChangeValidationWithoutArtifactClient(t *testing.T) {
	head := strings.Repeat("a", 40)
	receipt := func(timings int) gate.CommandResult {
		validation := gate.CommandResult{Command: "make check-land", HeadSHA: head, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Evidence: &gate.CommandEvidence{}, Output: strings.Repeat("validation passed 世界\n", 2500), Pipeline: &gate.PipelineEvidence{}}
		for i := range timings {
			timing := gate.PipelineTiming{Stage: "finalization", Command: "go test -short -timeout=60s ./internal/example", HeadSHA: head, TreeSHA: validation.TreeSHA, StartedAt: time.Unix(1, int64(i)).UTC(), FinishedAt: time.Unix(2, int64(i)).UTC(), ExitCode: new(0), Execution: "executed"}
			timing.ReceiptID = timing.ID()
			validation.Pipeline.Timings = append(validation.Pipeline.Timings, timing)
		}
		return validation
	}
	t.Run("medium receipt is stored bounded inline", func(t *testing.T) {
		validation := receipt(180)
		bounded := tracker.PublicGateResult(validation)
		boundedRaw, _ := json.Marshal(bounded)
		originalRaw, _ := json.Marshal(validation)
		if len(boundedRaw) <= 64<<10 || len(boundedRaw) > operatortool.MaxResultBytes || len(originalRaw) <= len(boundedRaw) {
			t.Fatalf("fixture sizes: bounded=%d original=%d", len(boundedRaw), len(originalRaw))
		}
		input := tracker.ChangeVersionInput{Validation: &validation}
		if err := (&nativeExecution{}).retainChangeValidation(t.Context(), &input); err != nil {
			t.Fatal(err)
		}
		if input.Validation == &validation || !reflect.DeepEqual(*input.Validation, bounded) {
			t.Fatal("inline fallback kept the unbounded receipt")
		}
		stored, err := json.Marshal(input)
		if err != nil || len(stored) > operatortool.MaxResultBytes {
			t.Fatalf("stored input exceeds the Change result limit: bytes=%d error=%v", len(stored), err)
		}
	})
	t.Run("receipt beyond the result limit requires an artifact service", func(t *testing.T) {
		validation := receipt(1024)
		input := tracker.ChangeVersionInput{Validation: &validation}
		if err := (&nativeExecution{}).retainChangeValidation(t.Context(), &input); !errors.Is(err, artifact.ErrMissing) || input.Validation != &validation {
			t.Fatalf("oversized receipt without artifact service: error=%v replaced=%t", err, input.Validation != &validation)
		}
	})
}

func TestRetainChangeValidationHonorsUploadPartLimit(t *testing.T) {
	const partLimit = 16 << 10
	head := strings.Repeat("a", 40)
	validation := gate.CommandResult{Command: "make check-land", HeadSHA: head, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Evidence: &gate.CommandEvidence{}, Output: "ok", Pipeline: &gate.PipelineEvidence{}}
	for i := range 1024 {
		timing := gate.PipelineTiming{Stage: "finalization", Command: "go test -short ./internal/世界", HeadSHA: head, TreeSHA: validation.TreeSHA, StartedAt: time.Unix(1, int64(i)).UTC(), FinishedAt: time.Unix(2, int64(i)).UTC(), ExitCode: new(0), Execution: "executed"}
		timing.ReceiptID = timing.ID()
		validation.Pipeline.Timings = append(validation.Pipeline.Timings, timing)
	}
	var parts []artifact.Part
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/uploads":
			_ = json.NewEncoder(w).Encode(artifact.Upload{ArtifactID: "artifact_" + strings.Repeat("a", 32), State: "uploading", PartLimit: partLimit})
		case strings.HasSuffix(r.URL.Path, "/parts"):
			var p artifact.Part
			if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
				t.Error(err)
			}
			if len(p.Data) > partLimit {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				_, _ = w.Write([]byte(`{"code":"quota_exceeded"}`))
				return
			}
			parts = append(parts, p)
			_ = json.NewEncoder(w).Encode(artifact.Object{})
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"code":"storage_unreachable"}`))
		}
	}))
	defer server.Close()
	e := &nativeExecution{artifacts: nativeArtifacts{client: &artifact.Client{Origin: server.URL, Token: func(context.Context) (string, error) { return "worker", nil }}, reservation: artifact.Reservation{Bytes: 4 << 20}}}
	input := tracker.ChangeVersionInput{Validation: &validation}
	if err := e.retainChangeValidation(t.Context(), &input); !errors.Is(err, artifact.ErrStorage) {
		t.Fatalf("finalize outage = %v", err)
	}
	full, err := json.Marshal(validation)
	if err != nil {
		t.Fatal(err)
	}
	var got []byte
	for index, p := range parts {
		if p.Sequence != index || len(p.Data) == 0 || len(p.Data) > partLimit || !utf8.Valid(p.Data) {
			t.Fatalf("part %d: sequence=%d bytes=%d", index, p.Sequence, len(p.Data))
		}
		got = append(got, p.Data...)
	}
	if len(parts) < 2 || !bytes.Equal(got, full) {
		t.Fatalf("parts=%d lost receipt bytes", len(parts))
	}
}

func TestRetainChangeValidationRejectsForeignServiceReference(t *testing.T) {
	head := strings.Repeat("a", 40)
	validation := gate.CommandResult{Command: "make check-land", HeadSHA: head, TreeSHA: strings.Repeat("d", 40), DurationNS: 1, Evidence: &gate.CommandEvidence{}, Output: "ok", Pipeline: &gate.PipelineEvidence{}}
	for i := range 1024 {
		timing := gate.PipelineTiming{Stage: "finalization", Command: "go test -short ./internal/example", HeadSHA: head, TreeSHA: validation.TreeSHA, StartedAt: time.Unix(1, int64(i)).UTC(), FinishedAt: time.Unix(2, int64(i)).UTC(), ExitCode: new(0), Execution: "executed"}
		timing.ReceiptID = timing.ID()
		validation.Pipeline.Timings = append(validation.Pipeline.Timings, timing)
	}
	full, err := json.Marshal(validation)
	if err != nil {
		t.Fatal(err)
	}
	artifactID := "artifact_" + strings.Repeat("a", 32)
	for _, test := range []struct {
		name      string
		serviceID string
		want      error
	}{
		{name: "reserved service is accepted", serviceID: "service_reserved"},
		{name: "foreign service is refused", serviceID: "service_foreign", want: artifact.ErrIntegrity},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/v1/uploads":
					_ = json.NewEncoder(w).Encode(artifact.Upload{ArtifactID: artifactID, State: "uploading"})
				case strings.HasSuffix(r.URL.Path, "/parts"):
					_ = json.NewEncoder(w).Encode(artifact.Object{})
				default:
					_ = json.NewEncoder(w).Encode(artifact.Reference{ServiceID: test.serviceID, ArtifactID: artifactID, Kind: "log", State: "complete", Availability: "available", Bytes: int64(len(full)), SHA256: artifact.Digest(full)})
				}
			}))
			defer server.Close()
			e := &nativeExecution{artifacts: nativeArtifacts{client: &artifact.Client{Origin: server.URL, Token: func(context.Context) (string, error) { return "worker", nil }}, reservation: artifact.Reservation{ServiceID: "service_reserved", Bytes: 4 << 20}}}
			input := tracker.ChangeVersionInput{Validation: &validation}
			err := e.retainChangeValidation(t.Context(), &input)
			if test.want == nil && (err != nil || input.Validation.EvidenceArtifact == nil || input.Validation.EvidenceArtifact.ServiceID != "service_reserved") {
				t.Fatalf("reserved service reference: error=%v validation=%+v", err, input.Validation.EvidenceArtifact)
			}
			if test.want != nil && (!errors.Is(err, test.want) || input.Validation != &validation) {
				t.Fatalf("foreign service reference: error=%v replaced=%t", err, input.Validation != &validation)
			}
		})
	}
}

func TestUploadChangeValidationBudgetsPartialManifests(t *testing.T) {
	for _, test := range []struct {
		name        string
		uploadBytes int64
		lines       int
	}{
		{name: "small admitted parts", uploadBytes: 16 << 10, lines: 30000},
		{name: "large receipt with large admitted parts", uploadBytes: 1 << 20, lines: 120000},
	} {
		t.Run(test.name, func(t *testing.T) {
			organization, serviceID := artifact.NewID("org"), artifact.NewID("service")
			service, err := artifact.NewService(t.Context(), artifact.Config{ServiceID: serviceID, OrganizationID: organization, Mode: "customer", DatabasePath: filepath.Join(t.TempDir(), "artifacts.db"), Policy: artifact.Policy{ID: "test", Limits: artifact.Limits{RetainedBytes: 64 << 20, ReservedBytes: 32 << 20, ArtifactBytes: 8 << 20, UploadBytes: test.uploadBytes, RetentionSeconds: 3600}, AbandonedUploadSeconds: 600, DeletionRecordSeconds: 7200, BackupSeconds: 3600}}, &changeEvidenceStorage{objects: map[string][]byte{}}, nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			raw := []byte(strings.Repeat("validation receipt 世界\n", test.lines))
			reservation := artifact.Reservation{ServiceID: serviceID, Mode: "customer", Scope: artifact.Scope{OrganizationID: organization, ProjectID: artifact.NewID("prj"), WorkItemID: artifact.NewID("wi"), RunID: artifact.NewID("run"), AttemptID: artifact.NewID("attempt")}, Key: "validation-budget", Kind: "log"}
			reference, err := uploadChangeValidation(t.Context(), service, reservation, raw)
			if err != nil {
				t.Fatalf("%d byte receipt: %v", len(raw), err)
			}
			if reference.Bytes != int64(len(raw)) || reference.ServiceID != reservation.ServiceID {
				t.Fatalf("reference = %+v", reference)
			}
		})
	}
}

func TestUploadChangeValidationRefusesUnfundedPartLimit(t *testing.T) {
	appended := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/uploads" {
			_ = json.NewEncoder(w).Encode(artifact.Upload{ArtifactID: "artifact_" + strings.Repeat("a", 32), State: "uploading", PartLimit: 1 << 10})
			return
		}
		appended++
		_ = json.NewEncoder(w).Encode(artifact.Object{})
	}))
	defer server.Close()
	client := &artifact.Client{Origin: server.URL, Token: func(context.Context) (string, error) { return "worker", nil }}
	_, err := uploadChangeValidation(t.Context(), client, artifact.Reservation{Kind: "log", Key: "validation"}, []byte(strings.Repeat("x", 64<<10)))
	if !errors.Is(err, artifact.ErrQuota) || appended != 0 {
		t.Fatalf("below-floor part limit: error=%v appended=%d", err, appended)
	}
}
