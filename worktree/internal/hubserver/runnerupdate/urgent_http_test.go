package runnerupdate

import (
	"bytes"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/runnerauth"
)

func TestUrgentRunnerUpdateReceiptSurvivesReplayExpiry(t *testing.T) {
	for _, format := range []string{"current"} {
		t.Run(format, func(t *testing.T) {
			start := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
			clock := hubtest.NewClock(start)
			f := newUpdateHTTPFixture(t, clock)
			path := "/api/v2/organizations/" + string(f.h.Organization) + "/runner-update/urgent"
			first := UrgentChange{Version: "1.2.4", Confirm: true, IdempotencyKey: "first"}
			response := performHubAPIRequest(t, f.h, http.MethodPut, path, f.admin.Token, first)
			requireNativeStatus(t, response, http.StatusAccepted)
			var urgent UrgentUpdate
			decodeHubResponse(t, response, &urgent)
			f.requireCommandObject(t, "urgent_runner_update", "first", response.Body.Bytes())

			requireNativeStatus(t, performHubAPIRequest(t, f.h, http.MethodPut, path, f.admin.Token, UrgentChange{ExpectedRevision: urgent.Revision, Version: "1.2.5", Confirm: true, IdempotencyKey: "second"}), http.StatusAccepted)
			clock.Set(start.Add(receiptstore.ReplayRetention + time.Second))
			var page []receiptstore.Record
			if err := f.h.Repository.ReadTenant(t.Context(), f.h.Organization, corestore.Background, func(r tenantstore.Reader) error {
				var err error
				page, err = r.(receiptstore.RetentionReader).ReceiptExpiryPage(t.Context(), clock.Now(), receiptstore.MaxRetentionBatch)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			for _, record := range page {
				if err := f.h.Repository.WriteTenant(t.Context(), f.h.Organization, corestore.Background, func(w tenantstore.Writer) error {
					_, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), record, clock.Now())
					return err
				}); err != nil {
					t.Fatal(err)
				}
			}

			requireNativeStatus(t, performHubAPIRequest(t, f.h, http.MethodPut, path, f.admin.Token, first), http.StatusGone)
			var expired bool
			var requestID, reconciliation, object string
			if err := f.database.QueryRowContext(t.Context(), `SELECT state.response_expired,state.response_request_id,state.response_reconciliation_hash,c.payload_object_key FROM native_command_states state JOIN native_commands c USING(organization_id,actor_id,operation,command_key) WHERE state.organization_id=$1 AND state.operation='urgent_runner_update' AND state.command_key='first'`, f.h.Organization).Scan(&expired, &requestID, &reconciliation, &object); err != nil {
				t.Fatal(err)
			}
			if !expired || requestID != urgent.Request.ID || reconciliation == "" || object == "" {
				t.Fatalf("expired urgent receipt lost typed provenance: expired=%v request=%s reconciliation=%s object=%s", expired, requestID, reconciliation, object)
			}
			var owners int
			if err := f.database.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners owner JOIN archive_objects object ON object.organization_id=owner.organization_id AND object.id=owner.archive_id WHERE owner.organization_id=$1 AND object.object_key=$2 AND owner.owner_kind='native-command'`, f.h.Organization, object).Scan(&owners); err != nil || owners != 0 {
				t.Fatalf("expired urgent replay retained payload owners=%d error=%v", owners, err)
			}
		})
	}
}

func TestUrgentRunnerUpdateHistoricalReceipt(t *testing.T) {
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	f := newUpdateHTTPFixture(t, hubtest.NewClock(now))
	path := "/api/v2/organizations/" + string(f.h.Organization) + "/runner-update/urgent"
	first := performHubAPIRequest(t, f.h, http.MethodPut, path, f.admin.Token, UrgentChange{Version: "1.2.4", Confirm: true, IdempotencyKey: "first"})
	requireNativeStatus(t, first, http.StatusAccepted)
	var urgent UrgentUpdate
	decodeHubResponse(t, first, &urgent)
	requireNativeStatus(t, performHubAPIRequest(t, f.h, http.MethodPut, path, f.admin.Token, UrgentChange{ExpectedRevision: urgent.Revision, Version: "1.2.5", Confirm: true, IdempotencyKey: "second"}), http.StatusAccepted)
	build := runnerauth.BuildEvidence{Version: "1.2.3", Commit: strings.Repeat("a", 40), Source: "release", SHA256: strings.Repeat("b", 64), OS: "linux", Architecture: "amd64", ObservedAt: now}
	for _, test := range []struct {
		name    string
		version string
		want    int
	}{{"forged history", "1.2.6", http.StatusConflict}, {"owned history", "1.2.4", http.StatusNoContent}} {
		t.Run(test.name, func(t *testing.T) {
			receipt := runnerauth.UpdateReceipt{Request: *urgent.Request, Status: "refused", FailureReason: "Update was not applied", ObservedAt: now}
			receipt.Request.Version = test.version
			requireNativeStatus(t, performHubAPIRequest(t, f.h, http.MethodPut, f.path+"/receipt", f.actor.Token, ReceiptWrite{Receipt: receipt, Running: build}), test.want)
			if test.want == http.StatusNoContent {
				response := performHubAPIRequest(t, f.h, http.MethodGet, f.path+"/receipt", f.actor.Token, nil)
				requireNativeStatus(t, response, http.StatusOK)
				var stored runnerauth.UpdateReceipt
				decodeHubResponse(t, response, &stored)
				if !reflect.DeepEqual(stored, receipt) {
					t.Fatalf("historical receipt=%+v want=%+v", stored, receipt)
				}
			}
		})
	}
}

func TestUrgentRunnerUpdateApplication(t *testing.T) {
	f := newUpdateHTTPFixture(t, hubtest.NewClock(time.Now().UTC()))
	path := "/api/v2/organizations/" + string(f.h.Organization) + "/runner-update/urgent"
	request := UrgentChange{Version: "v1.2.4", Confirm: true, IdempotencyKey: "urgent-release"}
	for _, test := range []struct {
		name       string
		credential string
		path       string
		change     func(*UrgentChange)
		want       int
	}{
		{"confirmation", f.admin.Token, path, func(r *UrgentChange) { r.Confirm = false }, http.StatusPreconditionRequired},
		{"worker", f.actor.Token, path, func(*UrgentChange) {}, http.StatusForbidden},
		{"other organization", f.admin.Token, "/api/v2/organizations/org_other/runner-update/urgent", func(*UrgentChange) {}, http.StatusUnauthorized},
		{"invalid release", f.admin.Token, path, func(r *UrgentChange) { r.Version = "dev" }, http.StatusUnprocessableEntity},
		{"stale revision", f.admin.Token, path, func(r *UrgentChange) { r.ExpectedRevision = 3 }, http.StatusConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := request
			test.change(&copy)
			requireNativeStatus(t, performHubAPIRequest(t, f.h, http.MethodPut, test.path, test.credential, copy), test.want)
		})
	}
	response := performHubAPIRequest(t, f.h, http.MethodPut, path, f.admin.Token, request)
	requireNativeStatus(t, response, http.StatusAccepted)
	var urgent UrgentUpdate
	decodeHubResponse(t, response, &urgent)
	if urgent.Revision != 1 || urgent.Request == nil || !urgent.Request.Urgent || urgent.Request.Validate() != nil {
		t.Fatalf("urgent release=%+v", urgent)
	}
	replay := performHubAPIRequest(t, f.h, http.MethodPut, path, f.admin.Token, request)
	requireNativeStatus(t, replay, http.StatusAccepted)
	if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("urgent replay changed")
	}
	request.Version = "1.2.5"
	requireNativeStatus(t, performHubAPIRequest(t, f.h, http.MethodPut, path, f.admin.Token, request), http.StatusConflict)
}
