package hubserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeObjectFixture struct {
	mu      sync.Mutex
	objects map[string][]byte
}

func (f *changeObjectFixture) call(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
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
		if int64(len(request.Data)) != request.Reference.CompressedSize || artifact.Digest(request.Data) != request.Reference.SHA256 {
			return nil, artifact.ErrIntegrity
		}
		if previous, found := f.objects[request.Reference.Key]; found && !bytes.Equal(previous, request.Data) {
			return nil, artifact.ErrIntegrity
		}
		f.objects[request.Reference.Key] = bytes.Clone(request.Data)
		return nil, nil
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
	cfg := Config{DatabasePath: filepath.Join(t.TempDir(), "hub.db"), content: objects.call}
	f := newChangeFixture(t, openTestService(t, cfg))
	ctx := f.service.contentContext(t.Context())
	input := changeTestInput()
	bundle := changeTestSource(t, &input, bytes.Repeat([]byte("retained source\n"), 20000))
	input.Validation = &gate.CommandResult{Command: "make check-land", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 123, Output: strings.Repeat("validation output\n", 3000)}
	versions := make([]tracker.ChangeVersion, 3)
	expected := ""
	for i := range versions {
		request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("objects-%d", i)}, ChangeVersionInput: input, SourceBundle: bundle, ExpectedVersionID: expected}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		decodeHubResponse(t, response, &versions[i])
		expected = versions[i].ID
	}
	output := strings.Repeat("full command output\n", 11000)
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
	}
	detail := f.detail(t)
	expectedVersions, _ := json.Marshal(versions)
	actualVersions, _ := json.Marshal(detail.Versions)
	expectedReviews, _ := json.Marshal(reviews)
	actualReviews, _ := json.Marshal(detail.Reviews)
	if !bytes.Equal(actualVersions, expectedVersions) {
		t.Fatalf("object storage changed version fields: got %s want %s", actualVersions, expectedVersions)
	}
	if !bytes.Equal(actualReviews, expectedReviews) {
		t.Fatal("object storage changed review fields")
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
		{"SELECT count(*) FROM change_content_objects", &count, 3},
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
	} {
		if err := f.service.database.reader.QueryRowContext(ctx, check.query).Scan(&maximum); err != nil {
			t.Fatal(err)
		}
		if maximum > 2048 {
			t.Fatalf("%s row uses %d bytes", check.table, maximum)
		}
	}

	requireUsageCountersMatchRecount(t, f.service.database)
	operatorCtx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	exact, err := (hubOperatorExecutor{f.service}).Execute(operatorCtx, changeToolCall(operatortool.GetChangeVersion, operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID, VersionID: versions[0].ID}))
	if err != nil {
		t.Fatal(err)
	}
	var exactVersion operatortool.ChangeResult
	if err := json.Unmarshal(exact.Content, &exactVersion); err != nil {
		t.Fatal(err)
	}
	if exactVersion.Version == nil || !reflect.DeepEqual(*exactVersion.Version, versions[0]) {
		t.Fatal("MCP exact version changed policy or validation output")
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
