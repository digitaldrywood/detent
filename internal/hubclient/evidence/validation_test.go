package evidence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubclient/internal/testsupport"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
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
		if err := Retain(t.Context(), &input, nil, artifact.Reservation{}, ""); err != nil {
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
		if err := Retain(t.Context(), &input, nil, artifact.Reservation{}, ""); !errors.Is(err, artifact.ErrMissing) || input.Validation != &validation {
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
	uploader := &artifact.Client{Origin: server.URL, Token: func(context.Context) (string, error) { return "worker", nil }}
	reservation := artifact.Reservation{Bytes: 4 << 20}
	input := tracker.ChangeVersionInput{Validation: &validation}
	if err := Retain(t.Context(), &input, uploader, reservation, ""); !errors.Is(err, artifact.ErrStorage) {
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
			uploader := &artifact.Client{Origin: server.URL, Token: func(context.Context) (string, error) { return "worker", nil }}
			reservation := artifact.Reservation{ServiceID: "service_reserved", Bytes: 4 << 20}
			input := tracker.ChangeVersionInput{Validation: &validation}
			err := Retain(t.Context(), &input, uploader, reservation, "")
			if test.want == nil && (err != nil || input.Validation.EvidenceArtifact == nil || input.Validation.EvidenceArtifact.ServiceID != "service_reserved") {
				t.Fatalf("reserved service reference: error=%v validation=%+v", err, input.Validation.EvidenceArtifact)
			}
			if test.want != nil && (!errors.Is(err, test.want) || input.Validation != &validation) {
				t.Fatalf("foreign service reference: error=%v replaced=%t", err, input.Validation != &validation)
			}
		})
	}
}

type authorizedValidationUploader struct {
	*artifact.Service
	t      *testing.T
	config hubserver.Config
}

func (u authorizedValidationUploader) Reserve(ctx context.Context, reservation artifact.Reservation) (artifact.Upload, error) {
	testsupport.NativeChangeWrite(u.t, u.config, func(writer tenantstore.Writer) error {
		return writer.AuthorizeArtifactUpload(ctx, reservation, u.config.Clock())
	})
	return u.Service.Reserve(ctx, reservation)
}

func TestUploadChangeValidationBudgetsPartialManifests(t *testing.T) {
	now := time.Now().UTC()
	clock := func() time.Time { return now }
	cfg := testsupport.NativeChangePostgresConfig(t, hubserver.Config{InitialAdminToken: []byte("example-admin"), Clock: clock, Content: testsupport.ContentStore()})
	hub, err := hubserver.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := hub.Close(); err != nil {
			t.Error(err)
		}
	})
	server := httptest.NewServer(hub.Handler())
	t.Cleanup(server.Close)
	admin, err := api.New(api.Config{URL: server.URL, TokenSource: func() string { return string(cfg.InitialAdminToken) }, HTTPClient: server.Client()})
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name        string
		uploadBytes int64
		lines       int
	}{
		{name: "small admitted parts", uploadBytes: 16 << 10, lines: 30000},
		{name: "large receipt with large admitted parts", uploadBytes: 1 << 20, lines: 120000},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := testsupport.NativeChangeProject(t, cfg, []tracker.NativeState{{Name: "Todo", Dispatchable: true}}, false)
			organization, serviceID := string(project.OrganizationID), artifact.NewID("service")
			publisherID, publisherToken := testsupport.NativeChangePublisher(t, cfg, project.ID)
			var active atomic.Pointer[artifact.HTTPServer]
			listener := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				handler := active.Load()
				if handler == nil {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				handler.ServeHTTP(w, request)
			}))
			t.Cleanup(listener.Close)
			scope := "/api/v2/organizations/" + organization + "/projects/" + string(project.ID)
			binding := artifact.Binding{ServiceID: serviceID, Origin: listener.URL, Mode: "customer", PublisherTokenID: publisherID}
			testsupport.NativeChangeWrite(t, cfg, func(writer tenantstore.Writer) error {
				return writer.BindArtifactService(t.Context(), project.ID, binding)
			})
			policy := artifact.Policy{ID: "test", Limits: artifact.Limits{RetainedBytes: 64 << 20, ReservedBytes: 32 << 20, ArtifactBytes: 8 << 20, UploadBytes: test.uploadBytes, RetentionSeconds: 3600}, AbandonedUploadSeconds: 600, DeletionRecordSeconds: 7200, BackupSeconds: 3600}
			if err := admin.Request(t.Context(), http.MethodPut, scope+"/artifact-services/"+serviceID+"/catalog-policy", policy, nil); err != nil {
				t.Fatal(err)
			}
			transport := &http.Transport{}
			t.Cleanup(transport.CloseIdleConnections)
			remote := &artifact.RemoteHub{Origin: server.URL, OrganizationID: organization, ProjectID: string(project.ID), ServiceID: serviceID, PublisherToken: func() string { return publisherToken }, Client: &http.Client{Transport: transport}}
			service, err := artifact.NewService(t.Context(), artifact.Config{ServiceID: serviceID, OrganizationID: organization, ProjectID: string(project.ID), Mode: "customer", Policy: policy, Catalog: &artifact.RemoteCatalog{Hub: remote}}, &changeEvidenceStorage{objects: map[string][]byte{}}, nil, artifact.ServiceOptions{Now: clock})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := service.Close(); err != nil {
					t.Error(err)
				}
			})
			handler, err := artifact.NewHTTPServer(service, remote)
			if err != nil {
				t.Fatal(err)
			}
			active.Store(handler)
			raw := []byte(strings.Repeat("validation receipt 世界\n", test.lines))
			reservation := artifact.Reservation{ServiceID: serviceID, Mode: "customer", Scope: artifact.Scope{OrganizationID: organization, ProjectID: string(project.ID), WorkItemID: artifact.NewID("wi"), RunID: artifact.NewID("run"), AttemptID: artifact.NewID("attempt")}, Key: "validation-budget", Kind: "log"}
			var lease leasestore.Lease
			testsupport.NativeChangeWrite(t, cfg, func(writer tenantstore.Writer) error {
				machine := artifact.NewID("machine")
				if err := writer.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: project.OrganizationID, ID: machine, Hostname: "example-worker", Capacity: 1}); err != nil {
					return err
				}
				item, err := writer.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: project.OrganizationID, ProjectID: project.ID, NativeID: tracker.NativeWorkItemID(reservation.WorkItemID), Number: 1, Revision: 1, Title: "Example validation", WorkflowStateName: new("Todo"), CreatedAt: clock(), UpdatedAt: clock(), NativeCreatedAt: clock(), NativeUpdatedAt: clock()})
				if err != nil {
					return err
				}
				lease, err = writer.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machine, SessionID: artifact.NewID("session"), LeaseID: artifact.NewID("lease"), TTL: time.Hour})
				if err != nil {
					return err
				}
				_, err = writer.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: reservation.AttemptID, Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, RunID: reservation.RunID, Sequence: 1, Status: "running", StartedAt: clock(), PayloadObjectKey: "example/start"})
				return err
			})
			reservation.LeaseID, reservation.FencingToken = lease.LeaseID, lease.FencingToken
			reference, err := Upload(t.Context(), authorizedValidationUploader{Service: service, t: t, config: cfg}, reservation, raw)
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
	_, err := Upload(t.Context(), client, artifact.Reservation{Kind: "log", Key: "validation"}, []byte(strings.Repeat("x", 64<<10)))
	if !errors.Is(err, artifact.ErrQuota) || appended != 0 {
		t.Fatalf("below-floor part limit: error=%v appended=%d", err, appended)
	}
}

func TestUploadChangeValidationRespectsConfiguredArtifactLimit(t *testing.T) {
	reserved := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reserved++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := &artifact.Client{Origin: server.URL, Token: func(context.Context) (string, error) { return "worker", nil }}
	_, err := Upload(t.Context(), client, artifact.Reservation{Kind: "log", Key: "validation", Bytes: 4 << 20}, []byte(strings.Repeat("x", 3<<20+512<<10)))
	if !errors.Is(err, artifact.ErrQuota) || reserved != 0 {
		t.Fatalf("receipt beyond the configured artifact limit: error=%v reserve calls=%d", err, reserved)
	}
}
