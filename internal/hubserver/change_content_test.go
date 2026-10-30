package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/pressly/goose/v3"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeObjectFixture struct {
	beforeRead  func(context.Context) error
	writes      int
	reads       int
	writerReads int
	mu          sync.Mutex
	objects     map[string][]byte
}

func (f *changeObjectFixture) call(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
	if !request.Write && f.beforeRead != nil {
		if err := f.beforeRead(ctx); err != nil {
			return nil, err
		}
	}
	if measurement, ok := ctx.Value(changeWriterMeasurementKey{}).(*changeWriterMeasurement); ok && measurement.active {
		measurement.content++
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Reference.Validate(request.Organization); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.objects == nil {
		f.objects = make(map[string][]byte)
	}
	if request.Write {
		f.writes++
		if int64(len(request.Data)) != request.Reference.CompressedSize || artifact.Digest(request.Data) != request.Reference.SHA256 {
			return nil, artifact.ErrIntegrity
		}
		if previous, found := f.objects[request.Reference.Key]; found && !bytes.Equal(previous, request.Data) {
			return nil, artifact.ErrIntegrity
		}
		f.objects[request.Reference.Key] = bytes.Clone(request.Data)
		return nil, nil
	}
	f.reads++
	if claimWriting(ctx) {
		f.writerReads++
		return nil, errors.New("content read while claim writer is held")
	}
	data, found := f.objects[request.Reference.Key]
	if !found {
		return nil, artifact.ErrStorage
	}
	return bytes.Clone(data), nil
}

func TestChangeContentObjects(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	now := time.Now().UTC()
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), Content: objects.call, now: func() time.Time { return now }}
	f := newChangeFixture(t, openTestService(t, cfg))
	legacy := f
	legacy.service = legacyChangeService(t, f.service)
	ctx := f.service.contentContext(t.Context())
	input := changeTestInput()
	bundle := changeTestSource(t, &input, bytes.Repeat([]byte("retained source\n"), 20000))
	input.Validation = &gate.CommandResult{Attribution: "unknown", Command: "make check-land", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 123, Output: strings.Repeat("validation output\n", 3000)}
	if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE projects SET require_visual_attachments=1 WHERE id=?", f.project.ID); err != nil {
		t.Fatal(err)
	}
	var initialRegistered int
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_content_objects").Scan(&initialRegistered); err != nil {
		t.Fatal(err)
	}
	objects.mu.Lock()
	initialWrites := objects.writes
	objects.mu.Unlock()
	for _, test := range []struct {
		name   string
		edit   func(*tracker.PublishChangeVersion)
		status int
	}{
		{"visual source refusal", func(request *tracker.PublishChangeVersion) {
			request.Artifacts = []tracker.ChangeArtifact{{Kind: "artifact", URI: "https://example.test/screen.png", SHA256: artifact.Digest([]byte("image")), Availability: "available"}}
		}, http.StatusUnprocessableEntity},
		{"stale version", func(request *tracker.PublishChangeVersion) {
			request.ExpectedVersionID = newNativeID("version")
		}, http.StatusConflict},
		{"unknown configuration", func(request *tracker.PublishChangeVersion) {
			request.PolicyID = artifact.Digest([]byte("unknown configuration"))
		}, http.StatusUnprocessableEntity},
		{"invalid validation artifact", func(request *tracker.PublishChangeVersion) {
			validation := *request.Validation
			validation.EvidenceArtifact = &artifact.Reference{}
			request.Validation = &validation
		}, http.StatusUnprocessableEntity},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: test.name}, ChangeVersionInput: input, SourceBundle: bundle}
			test.edit(&request)
			refused := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
			requireNativeStatus(t, refused, test.status)
			var registered int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM change_content_objects").Scan(&registered); err != nil {
				t.Fatal(err)
			}
			objects.mu.Lock()
			writes := objects.writes
			objects.mu.Unlock()
			if registered != initialRegistered || writes != initialWrites {
				t.Fatalf("refusal published archives: registered=%d writes=%d", registered-initialRegistered, writes-initialWrites)
			}
		})
	}

	versions := make([]tracker.ChangeVersion, 3)
	expected := ""
	for i := range versions {
		request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("objects-%d", i)}, ChangeVersionInput: input, SourceBundle: bundle, ExpectedVersionID: expected}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		var published tracker.ChangeVersion
		decodeHubResponse(t, response, &published)
		versions[i] = tracker.ChangeVersion{ChangeVersionInput: input, ID: published.ID, ChangeID: f.change.ID, Number: int64(f.change.Revision) + int64(i), Policy: hubTestPolicy(), ReviewPolicy: f.rules, Actor: tracker.Actor{Kind: "human", PrincipalID: f.rules.RequiredChecks[0].PrincipalID}, CreatedAt: now}
		for position, spec := range f.rules.RequiredChecks {
			versions[i].Checks = append(versions[i].Checks, tracker.ChangeCheckExpectation{ChangeCheckSpec: spec, CheckRunID: published.Checks[position].CheckRunID})
		}
		if !reflect.DeepEqual(published, versions[i]) {
			t.Fatal("publication differs from the input's legacy version record")
		}
		raw, err := json.Marshal(versions[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.service.database.db.ExecContext(ctx, `INSERT INTO change_versions(id,change_id,number,record_json) VALUES(?,?,?,?)`, versions[i].ID, f.change.ID, versions[i].Number, string(raw)); err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.service.database.db.ExecContext(ctx, `INSERT INTO change_sources(version_id,bundle) VALUES(?,?)`, versions[i].ID, bundle); err != nil {
			t.Fatal(err)
		}
		expected = versions[i].ID
	}
	legacy.change.CurrentVersion, legacy.change.UpdatedAt = expected, now
	legacy.change.Revision += 3
	rawChange, err := json.Marshal(legacy.change)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.service.database.db.ExecContext(ctx, `UPDATE change_requests SET record_json=? WHERE id=?`, string(rawChange), f.change.ID); err != nil {
		t.Fatal(err)
	}
	output := strings.Repeat("full command output\n", 11000)[:200<<10]
	reviews := make([]tracker.ChangeReview, 2)
	for i := range reviews {
		reviews[i] = tracker.ChangeReview{ID: newNativeID("review"), VersionID: versions[2].ID, Decision: "commented", Body: "Findings remain inline", CreatedAt: f.service.config.now(), Validator: &gate.ValidatorResult{VersionID: versions[2].ID, Repository: input.Repository, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, Verdict: "wait", NotVerified: []string{"Unresolved evidence question"}, Commands: []gate.CommandResult{{Command: "make check-land", Output: output, HeadSHA: input.HeadSHA, TreeSHA: input.BaseSHA, ExitCode: 1, DurationNS: 123}}}}
		if _, err := prepareChangeContent(ctx, string(f.project.OrganizationID), []byte(output)); err != nil {
			t.Fatal(err)
		}
		err := f.service.hubTransact(ctx, func(tx *sql.Tx, _ time.Time) error {
			return storeChangeReview(ctx, tx, string(f.project.OrganizationID), f.change.ID, reviews[i])
		})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := json.Marshal(reviews[i])
		if err != nil {
			t.Fatal(err)
		}
		if _, err := legacy.service.database.db.ExecContext(ctx, `INSERT INTO change_evidence(change_id,version_id,kind,record_json) VALUES(?,?,'review',?)`, f.change.ID, reviews[i].VersionID, string(raw)); err != nil {
			t.Fatal(err)
		}
	}
	detail := f.detail(t)
	if !reflect.DeepEqual(detail.Versions, versions) || !reflect.DeepEqual(detail.Reviews, reviews) || !reflect.DeepEqual(detail, legacy.detail(t)) {
		t.Fatal("Change API differs from independently seeded legacy records")
	}
	var count, policies, maximum int
	for _, check := range []struct {
		query  string
		target *int
		want   int
	}{
		{"SELECT count(*) FROM change_sources WHERE length(bundle)=0 AND object_key<>''", &count, 3},
		{"SELECT count(DISTINCT policy_revision_id) FROM change_versions WHERE policy_revision_id<>'' AND json_extract(record_json,'$.policy') IS NULL", &policies, 1},
		{"SELECT count(DISTINCT object_key) FROM change_command_outputs", &count, 2},
		{"SELECT count(*) FROM change_content_objects", &count, initialRegistered + 8 + len(versions)},
	} {
		if err := f.service.database.reader.QueryRowContext(ctx, check.query).Scan(check.target); err != nil {
			t.Fatal(err)
		}
		if *check.target != check.want {
			t.Fatalf("%s = %d, want %d", check.query, *check.target, check.want)
		}
	}
	for _, check := range []struct{ table, query string }{
		{"change_versions", `SELECT max(length(CAST(record_json AS BLOB))+length(id)+length(change_id)+length(policy_revision_id)+16) FROM change_versions`},
		{"change_evidence", `SELECT max(length(CAST(record_json AS BLOB))+length(change_id)+length(version_id)+length(kind)+16) FROM change_evidence`},
		{"change_sources", `SELECT max(length(version_id)+length(bundle)+length(object_key)+length(object_sha256)+16) FROM change_sources`},
	} {
		if err := f.service.database.reader.QueryRowContext(ctx, check.query).Scan(&maximum); err != nil {
			t.Fatal(err)
		}
		if maximum > 2048 {
			t.Fatalf("%s row uses %d bytes", check.table, maximum)
		}
	}
	objects.mu.Lock()
	objectCount := len(objects.objects)
	objects.mu.Unlock()
	if objectCount != initialRegistered+8+len(versions) {
		t.Fatalf("stored %d objects, want fixture archives, one bundle, two command outputs, three versions, two reviews and three command receipts", objectCount)
	}

	requireUsageCountersMatchRecount(t, f.service.database)
	legacy.service.config.Content = objects.call
	conversionContext := legacy.service.contentContext(t.Context())
	conversionScope := nativeScope{organization: legacy.project.OrganizationID, project: legacy.project.ID}
	for _, family := range storageConversionFamilies {
		if err := legacy.service.convertStorageFamily(conversionContext, conversionScope, family, now); err != nil {
			t.Fatal(err)
		}
	}
	if !reflect.DeepEqual(detail, legacy.detail(t)) {
		t.Fatal("legacy conversion changed Change API")
	}
	operatorCtx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	legacyOperatorCtx := changeOperatorContext(t, legacy.service, f.token, string(f.project.OrganizationID))
	for _, args := range []operatortool.ChangeArguments{
		{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID, VersionID: versions[0].ID},
		{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID, Section: "reviews"},
	} {
		tool := operatortool.GetChange
		if args.VersionID != "" {
			tool = operatortool.GetChangeVersion
		}
		actual, err := (hubOperatorExecutor{f.service}).Execute(operatorCtx, changeToolCall(tool, args))
		if err != nil {
			t.Fatal(err)
		}
		want, err := (hubOperatorExecutor{legacy.service}).Execute(legacyOperatorCtx, changeToolCall(tool, args))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(actual.Content, want.Content) {
			t.Fatalf("%s differs between object references and inline legacy records", tool)
		}
	}

	response := performHubAPIRequest(t, f.service, http.MethodGet, f.path+"/versions/"+versions[0].ID+"/source", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), bundle) {
		t.Fatal("retained source bytes changed")
	}
	stored, err := nativeChangeSourceStored(ctx, f.service.database.reader, versions[0])
	if err != nil || !stored {
		t.Fatalf("restorable source unavailable: %v", err)
	}
	cursor := recoveryEvidenceCursor{Change: f.change.ID}
	_, _, version, raw, err := f.service.readRecoveryEvidenceRecord(ctx, f.service.database.reader, nativeScope{organization: f.project.OrganizationID, project: f.project.ID}, string(f.issue.WorkItemID), "reviews", cursor)
	if err != nil {
		t.Fatal(err)
	}
	var recovered tracker.ChangeReview
	if err := json.Unmarshal(raw, &recovered); err != nil {
		t.Fatal(err)
	}
	if version != versions[2].ID || !reflect.DeepEqual(recovered.Validator, reviews[0].Validator) || recovered.ID != reviews[0].ID {
		t.Fatal("recovery lost full review output or exact version")
	}
	if err := f.service.Close(); err != nil {
		t.Fatal(err)
	}
	f.service = openTestService(t, cfg)
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.path+"/versions/"+versions[0].ID+"/source", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), bundle) {
		t.Fatal("source was not durable across restart")
	}
	ctx = f.service.contentContext(t.Context())
	alpha, err := prepareChangeContent(ctx, "org_alpha", []byte(output))
	if err != nil {
		t.Fatal(err)
	}
	beta, err := prepareChangeContent(ctx, "org_beta", []byte(output))
	if err != nil {
		t.Fatal(err)
	}
	if alpha.Key == beta.Key || alpha.SHA256 != beta.SHA256 {
		t.Fatal("org object namespaces are not isolated")
	}
	if _, err := readChangeContent(ctx, "org_beta", alpha); err == nil {
		t.Fatal("foreign organization read accepted")
	}
	for _, check := range []struct {
		org string
		ref attachment.ArchiveReference
	}{{"org_alpha", alpha}, {"org_beta", beta}} {
		data, err := readChangeContent(ctx, check.org, check.ref)
		if err != nil || string(data) != output {
			t.Fatalf("org content unavailable: %v", err)
		}
	}
}

func legacyChangeService(t *testing.T, service *Service) *Service {
	t.Helper()
	path := filepath.Join(t.TempDir(), "legacy.db")
	if err := service.Backup(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	return openTestService(t, Config{DatabasePath: path, now: service.config.now, Content: service.config.Content, RunDetails: service.config.RunDetails})
}

func TestAttachmentReceiptSharedContentBilling(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name     string
		billable []bool
	}{
		{"receipt first", []bool{false, true, false}},
		{"content first", []bool{true, false, true}},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newHostedSharedFixture(t)
			hostedTestPlans(t, f.service, map[string]int64{"collaboration_bytes": 10000})
			ctx := f.service.contentContext(t.Context())
			var charged bool
			for _, billable := range test.billable {
				ref, err := prepareMeteredContent(ctx, "org_security", []byte("shared attachment metadata"), billable)
				if err != nil {
					t.Fatal(err)
				}
				charged = charged || billable
				var want int64
				if charged {
					want = ref.CompressedSize
				}
				var used int64
				if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&used); err != nil || used != want {
					t.Fatalf("billable=%t usage=%d want=%d err=%v", billable, used, want, err)
				}
			}
		})
	}
}

func TestAttachmentReceiptBillingMigration(t *testing.T) {
	t.Parallel()
	f := newHostedSharedFixture(t)
	const file = "20261030200758_attachment_receipt_billing.sql"
	data, err := migrationFiles.ReadFile("migrations/" + file)
	if err != nil {
		t.Fatal(err)
	}
	const ledger = "attachment_receipt_billing_test"
	provider, err := goose.NewProvider(goose.DialectSQLite3, f.service.database.db, fstest.MapFS{file: &fstest.MapFile{Data: data}}, goose.WithDisableGlobalRegistry(true), goose.WithTableName(ledger), goose.WithSlog(discardLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := provider.GetVersions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO "+ledger+" (version_id,is_applied) VALUES (20261030200758,1)"); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.Down(t.Context()); err != nil {
		t.Fatal(err)
	}
	var total int64
	for _, operation := range []string{"attachment.record example", "example.record", "attachment.record shared"} {
		ref, _, err := attachment.PrepareArchive("org_security", 1, strings.NewReader(operation))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO change_content_objects(organization_id,object_sha256,object_key,object_bytes,content_bytes) VALUES(?,?,?,?,?)", "org_security", ref.SHA256, ref.Key, ref.CompressedSize, ref.UncompressedSize); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,response_object_key,created_at) VALUES(?,?,?,?,?,'{}',?,?)", "org_security", bootstrapTokenID, operation, operation, "example", ref.Key, testTimestamp); err != nil {
			t.Fatal(err)
		}
		total += ref.CompressedSize
		if operation == "attachment.record shared" {
			if _, err := f.service.database.db.ExecContext(t.Context(), "INSERT INTO native_commands(organization_id,actor_id,operation,command_key,request_hash,response_json,response_object_key,created_at) VALUES(?,?,'example.shared','shared','example','{}',?,?)", "org_security", bootstrapTokenID, ref.Key, testTimestamp); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := provider.Up(t.Context()); err != nil {
		t.Fatal(err)
	}
	var sharedBillable bool
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT billable FROM change_content_objects WHERE object_key=(SELECT response_object_key FROM native_commands WHERE operation='example.shared')").Scan(&sharedBillable); err != nil || !sharedBillable {
		t.Errorf("shared command content became unbillable: billable=%t err=%v", sharedBillable, err)
	}
	var usage int64
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&usage); err != nil || usage != total {
		t.Fatalf("migration reclassified history: usage=%d want=%d err=%v", usage, total, err)
	}
	if err := f.service.convertLegacyStorage(t.Context(), f.service.config.now()); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&usage); err != nil || usage != total {
		t.Fatalf("background reclassified history: usage=%d want=%d err=%v", usage, total, err)
	}
	if err := f.service.hubTransact(t.Context(), func(tx *sql.Tx, _ time.Time) error {
		return recountCollaborationBytes(t.Context(), tx)
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&usage); err != nil || usage != total {
		t.Fatalf("recounted receipt usage=%d want=%d err=%v", usage, total, err)
	}
}
