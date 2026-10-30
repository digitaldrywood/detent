package hubclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestStepPayloadTransferRecovery(t *testing.T) {
	for _, scenario := range []string{"slow", "manifest interrupted", "object interrupted"} {
		t.Run(scenario, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				receipt := tracker.NativeStepReceipt{Step: "commit", CompletedAt: now, PolicyID: "policy", Source: tracker.NativeCheckpoint{HeadSHA: strings.Repeat("a", 40), WorkspaceDigest: strings.Repeat("b", 64)}}
				content, err := json.Marshal(receipt)
				if err != nil {
					t.Fatal(err)
				}
				scope := artifact.Scope{OrganizationID: "org_" + strings.Repeat("a", 32), ProjectID: "prj_" + strings.Repeat("b", 32), WorkItemID: "wi_" + strings.Repeat("c", 32), RunID: "run_" + strings.Repeat("1", 32), AttemptID: "attempt_" + strings.Repeat("2", 32)}
				manifest := artifact.Manifest{SchemaVersion: 1, Scope: scope, ArtifactID: "artifact_" + strings.Repeat("d", 32), ManifestID: "manifest_" + strings.Repeat("e", 32), Revision: 1, Kind: "log", State: "complete", CreatedAt: now, ExpiresAt: now.Add(time.Hour), RetentionPolicyID: "policy", TotalBytes: int64(len(content)), Objects: []artifact.Object{{ID: "object_" + strings.Repeat("f", 32), MediaType: "text/plain; charset=utf-8", Size: int64(len(content)), SHA256: artifact.Digest(content)}}}
				if err := manifest.Validate(); err != nil {
					t.Fatal(err)
				}
				raw, err := json.Marshal(manifest)
				if err != nil {
					t.Fatal(err)
				}
				bounded := receipt
				bounded.PayloadArtifact = &artifact.Reference{Scope: scope, ArtifactID: manifest.ArtifactID, Revision: 1, SHA256: artifact.Digest(raw), Kind: "log", State: "complete", Availability: "available", Bytes: int64(len(content))}
				failed := false
				transport := executionRoundTrip(func(request *http.Request) (*http.Response, error) {
					data := raw
					object := strings.Contains(request.URL.Path, "/objects/")
					if strings.HasSuffix(request.URL.Path, "/access") {
						data, _ = json.Marshal(artifact.Grant{Origin: "http://localhost", SHA256: artifact.Digest(raw), Token: "member"})
					} else {
						if !failed && (scenario == "manifest interrupted" && !object || scenario == "object interrupted" && object) {
							failed = true
							return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(failedReceiptBody{})}, nil
						}
						timer := time.NewTimer(31 * time.Second)
						defer timer.Stop()
						select {
						case <-timer.C:
						case <-request.Context().Done():
							return nil, request.Context().Err()
						}
						if object {
							data = content
						}
					}
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(data))}, nil
				})
				client, err := New(Config{URL: "http://localhost", TokenSource: func() string { return "worker" }, HTTPClient: &http.Client{Transport: transport}})
				if err != nil {
					t.Fatal(err)
				}
				native, _ := client.Native(tracker.OrganizationID(scope.OrganizationID), tracker.ProjectID(scope.ProjectID))
				e := &nativeExecution{claim: nativeClaim{source: &NativeConnector{client: native}, lease: tracker.NativeLease{WorkItemID: tracker.NativeWorkItemID(scope.WorkItemID)}}}
				ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
				defer cancel()
				restored, err := e.restoreStep(ctx, bounded)
				if scenario != "slow" {
					if !errors.Is(err, artifact.ErrStorage) {
						t.Fatalf("read interruption = %v", err)
					}
					restored, err = e.restoreStep(ctx, bounded)
				}
				if err != nil || !reflect.DeepEqual(restored, receipt) {
					t.Fatalf("receipt was not recovered: %v", err)
				}
			})
		})
	}
}

type failedReceiptBody struct{}

func (failedReceiptBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }
