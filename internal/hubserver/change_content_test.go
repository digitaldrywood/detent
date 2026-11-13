package hubserver

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/projectsettings"
	"github.com/digitaldrywood/detent/internal/tracker"
)

var changeContentCursorPattern = regexp.MustCompile(`"next_cursor":"([A-Za-z0-9_-]+)"`)

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

func (f *changeObjectFixture) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	ref, compressed, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err == nil {
		_, err = f.call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref, Data: compressed, Write: true})
	}
	return ref, err
}

func (f *changeObjectFixture) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	compressed, err := f.call(ctx, entryauthority.ContentRequest{Organization: organization, Reference: ref})
	if err != nil {
		return nil, err
	}
	if int64(len(compressed)) != ref.CompressedSize || artifact.Digest(compressed) != ref.SHA256 {
		return nil, artifact.ErrIntegrity
	}
	reader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(reader, ref.UncompressedSize+1))
	if err = errors.Join(err, reader.Close()); err != nil {
		return nil, err
	}
	if int64(len(raw)) != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return raw, nil
}

func TestChangeContentObjects(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	now := time.Now().UTC().Truncate(time.Microsecond)
	cfg := Config{Content: objects.call, RunDetails: objects, now: func() time.Time { return now }}
	f := newChangeFixture(t, newPostgresNativeService(t, cfg))
	admin := hostedPostgresAdmin(t, f.service)
	scope := analyticsTestScope(t, f.nativeFixture)
	ctx := f.service.contentContext(t.Context())
	input := changeTestInput()
	bundle := changeTestSource(t, &input, bytes.Repeat([]byte("retained source\n"), 20000))
	input.Validation = &gate.CommandResult{Attribution: "unknown", Command: "make check-land", HeadSHA: input.HeadSHA, TreeSHA: strings.Repeat("d", 40), DurationNS: 123, Output: strings.Repeat("validation output\n", 3000)}
	settings := cloudContentSettingsRequest{Mutation: tracker.Mutation{IdempotencyKey: "require-visual"}, ContentChange: projectsettings.ContentChange{RequireVisualAttachments: true}}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, f.base+"/content-settings", testHubAdminToken, settings), http.StatusOK)
	var initialRegistered int
	if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&initialRegistered); err != nil {
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
		}, http.StatusConflict},
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
			if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_objects WHERE organization_id=$1", f.project.OrganizationID).Scan(&registered); err != nil {
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

	var usedBefore, recountedBefore int64
	if err := f.service.config.TenantRepository.ReadTenant(ctx, f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		if usedBefore, err = r.CollaborationBytes(ctx); err != nil {
			return err
		}
		return admin.QueryRowContext(ctx, `SELECT COALESCE(sum(collaboration_bytes),0) FROM archive_objects WHERE organization_id=$1`, f.project.OrganizationID).Scan(&recountedBefore)
	}); err != nil {
		t.Fatal(err)
	}
	versions := make([]tracker.ChangeVersion, 3)
	expected := ""
	for i := range versions {
		request := tracker.PublishChangeVersion{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("objects-%d", i)}, ChangeVersionInput: input, SourceBundle: bundle, ExpectedVersionID: expected}
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, request)
		requireNativeStatus(t, response, http.StatusOK)
		var published tracker.ChangeVersion
		decodeHubResponse(t, response, &published)
		versions[i] = tracker.ChangeVersion{ChangeVersionInput: input, ID: published.ID, ChangeID: f.change.ID, Number: int64(f.change.Revision) + int64(i), Policy: hubTestPolicy(), ReviewPolicy: f.rules, Actor: scope.actor(), CreatedAt: now}
		for position, spec := range f.rules.RequiredChecks {
			versions[i].Checks = append(versions[i].Checks, tracker.ChangeCheckExpectation{ChangeCheckSpec: spec, CheckRunID: published.Checks[position].CheckRunID})
		}
		if !reflect.DeepEqual(published, versions[i]) {
			t.Fatal("publication differs from the independently prepared version record")
		}
		expected = versions[i].ID
	}
	output := strings.Repeat("full command output\n", 11000)[:200<<10]
	reviews := make([]tracker.ChangeReview, 2)
	for i := range reviews {
		reviews[i] = tracker.ChangeReview{ID: newNativeID("review"), VersionID: versions[2].ID, Decision: "commented", Body: "Findings remain inline", CreatedAt: f.service.config.now(), Validator: &gate.ValidatorResult{VersionID: versions[2].ID, Repository: input.Repository, BaseSHA: input.BaseSHA, HeadSHA: input.HeadSHA, Verdict: "wait", NotVerified: []string{"Unresolved evidence question"}, Commands: []gate.CommandResult{{Command: "make check-land", Output: output, HeadSHA: input.HeadSHA, TreeSHA: input.BaseSHA, ExitCode: 1, DurationNS: 123}}}}
		if _, err := prepareChangeContent(ctx, string(f.project.OrganizationID), []byte(output)); err != nil {
			t.Fatal(err)
		}
		row, err := eventstore.NewContent(objects).PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: f.project.OrganizationID, ChangeID: f.change.ID, Kind: "review"}, historyTestJSON(t, reviews[i]))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.config.TenantRepository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.AppendEvidence(ctx, row)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	detail := f.detail(t)
	if !reflect.DeepEqual(detail.Versions, versions) || !reflect.DeepEqual(detail.Reviews, reviews) {
		t.Fatal("Change API differs from independently prepared version and review records")
	}
	var sources, policies int
	if err := admin.QueryRowContext(ctx, `SELECT count(*),count(DISTINCT policy_revision_id) FROM change_versions WHERE organization_id=$1 AND change_id=$2 AND source_object_key<>''`, f.project.OrganizationID, f.change.ID).Scan(&sources, &policies); err != nil || sources != 3 || policies != 1 {
		t.Fatalf("retained sources=%d policies=%d want=3/1 error=%v", sources, policies, err)
	}
	var sourceKeys int
	if err := admin.QueryRowContext(ctx, `SELECT count(DISTINCT source_object_key) FROM change_versions WHERE organization_id=$1 AND change_id=$2`, f.project.OrganizationID, f.change.ID).Scan(&sourceKeys); err != nil || sourceKeys != 1 {
		t.Fatalf("identical source bundles registered=%d want=1 error=%v", sourceKeys, err)
	}
	for _, table := range []string{"change_requests", "change_versions", "change_evidence"} {
		var maximum int
		if err := admin.QueryRowContext(ctx, "SELECT COALESCE(max(pg_column_size(r)),0) FROM "+table+" r WHERE organization_id=$1", f.project.OrganizationID).Scan(&maximum); err != nil {
			t.Fatal(err)
		}
		if maximum == 0 || maximum > 2048 {
			t.Fatalf("%s row uses %d bytes", table, maximum)
		}
	}
	shared, err := prepareChangeContent(ctx, string(f.project.OrganizationID), []byte(output))
	if err != nil {
		t.Fatal(err)
	}
	objects.mu.Lock()
	beforeShared := len(objects.objects)
	objects.mu.Unlock()
	repeated, err := prepareChangeContent(f.service.contentContext(t.Context()), string(f.project.OrganizationID), []byte(output))
	if err != nil || repeated != shared {
		t.Fatalf("shared command output reference changed: before=%+v after=%+v error=%v", shared, repeated, err)
	}
	objects.mu.Lock()
	afterShared := len(objects.objects)
	objects.mu.Unlock()
	if afterShared != beforeShared {
		t.Fatalf("shared command output duplicated objects: before=%d after=%d", beforeShared, afterShared)
	}
	if err := f.service.config.TenantRepository.ReadTenant(ctx, f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		used, err := r.CollaborationBytes(ctx)
		if err != nil {
			return err
		}
		var recounted int64
		if err := admin.QueryRowContext(ctx, `SELECT COALESCE(sum(collaboration_bytes),0) FROM archive_objects WHERE organization_id=$1`, f.project.OrganizationID).Scan(&recounted); err != nil {
			return err
		}
		if used-usedBefore != recounted-recountedBefore || recounted == recountedBefore {
			t.Fatalf("collaboration bytes counter=%d recount=%d before=%d/%d", used, recounted, usedBefore, recountedBefore)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	fixture, historical, importedVersions := historyRepairSource(t, f, versions[0], len(versions))
	historical.change = detail.Change
	historical.change.ID = importedVersions[0].ChangeID
	historical.change.CurrentVersion = importedVersions[len(importedVersions)-1].ID
	fixture.records["change_requests"] = nil
	fixture.add("change_requests", map[string]any{"id": historical.change.ID, "organization_id": historical.project.OrganizationID, "project_id": historical.project.ID, "work_item_id": historical.issue.WorkItemID, "record_json": string(historyTestJSON(t, historical.change))})
	fixture.records["change_versions"] = nil
	mapping := []string{}
	for i := range importedVersions {
		id := importedVersions[i].ID
		importedVersions[i] = versions[i]
		importedVersions[i].ID, importedVersions[i].ChangeID = id, historical.change.ID
		fixture.add("change_versions", map[string]any{"id": id, "change_id": historical.change.ID, "number": importedVersions[i].Number, "policy_revision_id": importedVersions[i].PolicyID, "record_json": string(historyTestJSON(t, importedVersions[i]))})
		mapping = append(mapping, id, versions[i].ID)
	}
	mapping = append(mapping, historical.change.ID, f.change.ID)
	for i, original := range reviews {
		review := original
		review.VersionID = importedVersions[2].ID
		validator := *review.Validator
		validator.VersionID = review.VersionID
		review.Validator = &validator
		fixture.add("change_evidence", map[string]any{"sequence": i + 1, "change_id": historical.change.ID, "version_id": review.VersionID, "kind": "review", "record_json": string(historyTestJSON(t, review))})
	}
	source := historyRepairImportSource(t, historical, fixture)
	for range 2 {
		historyRepairImport(t, historical, source, 1)
		want, got := historyTestJSON(t, detail), historyTestJSON(t, historical.detail(t))
		if !bytes.Equal(want, []byte(strings.NewReplacer(mapping...).Replace(string(got)))) {
			t.Fatal("verified historical conversion changed Change API")
		}
	}
	operatorCtx := changeOperatorContext(t, f.service, f.token, string(f.project.OrganizationID))
	historicalOperatorCtx := changeOperatorContext(t, historical.service, historical.token, string(historical.project.OrganizationID))
	for _, args := range []operatortool.ChangeArguments{
		{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID, VersionID: versions[0].ID},
		{ProjectID: string(f.project.ID), ItemID: string(f.issue.WorkItemID), ChangeID: f.change.ID, Section: "reviews"},
	} {
		tool := operatortool.GetChange
		historicalArgs := args
		historicalArgs.ChangeID = historical.change.ID
		if args.VersionID != "" {
			tool = operatortool.GetChangeVersion
			historicalArgs.VersionID = importedVersions[0].ID
		}
		actual, err := (hubOperatorExecutor{f.service}).Execute(operatorCtx, changeToolCall(tool, args))
		if err != nil {
			t.Fatal(err)
		}
		want, err := (hubOperatorExecutor{historical.service}).Execute(historicalOperatorCtx, changeToolCall(tool, historicalArgs))
		if err != nil {
			t.Fatal(err)
		}
		replacer := strings.NewReplacer(mapping...)
		openCursors := func(raw []byte) []byte {
			return changeContentCursorPattern.ReplaceAllFunc(raw, func(match []byte) []byte {
				decoded, err := base64.RawURLEncoding.DecodeString(string(changeContentCursorPattern.FindSubmatch(match)[1]))
				if err != nil {
					t.Fatalf("%s returned an unreadable continuation: %v", tool, err)
				}
				return []byte(`"next_cursor":` + strconv.Quote(replacer.Replace(string(decoded))))
			})
		}
		current, mapped := openCursors(actual.Content), openCursors([]byte(replacer.Replace(string(want.Content))))
		if !bytes.Equal(current, mapped) {
			at := 0
			for at < len(current) && at < len(mapped) && current[at] == mapped[at] {
				at++
			}
			t.Fatalf("%s differs between current and independently imported historical records at byte %d: current=%q historical=%q", tool, at, current[max(0, at-120):min(len(current), at+120)], mapped[max(0, at-120):min(len(mapped), at+120)])
		}
	}

	response := performHubAPIRequest(t, f.service, http.MethodGet, f.path+"/versions/"+versions[0].ID+"/source", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), bundle) {
		t.Fatal("retained source bytes changed")
	}
	f.service = reopenCoordinatorService(t, f.service, func(cfg *Config) { cfg.Hosted.OrganizationID = "org_startup" })
	response = performHubAPIRequest(t, f.service, http.MethodGet, f.path+"/versions/"+versions[0].ID+"/source", f.token, nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Equal(response.Body.Bytes(), bundle) {
		t.Fatal("source was not durable across restart")
	}
	ctx = f.service.contentContext(t.Context())
	for _, org := range []tracker.OrganizationID{"org_alpha", "org_beta"} {
		if err := f.service.config.TenantRepository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
			return w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), Slug: strings.ReplaceAll(string(org), "_", "-"), CreatedAt: now})
		}); err != nil {
			t.Fatal(err)
		}
	}
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
	t.Run("unconfigured storage", func(t *testing.T) {
		f.service.config.Content, f.service.config.RunDetails = nil, nil
		t.Cleanup(func() { f.service.config.Content, f.service.config.RunDetails = objects.call, objects })
		for _, path := range []string{f.path, f.path + "/versions/" + versions[0].ID + "/source"} {
			response := performHubAPIRequest(t, f.service, http.MethodGet, path, f.token, nil)
			requireNativeStatus(t, response, http.StatusServiceUnavailable)
			var failure apiErrorResponse
			decodeHubResponse(t, response, &failure)
			if failure.Code != "storage_unreachable" {
				t.Fatalf("converted content read without storage = %+v", failure)
			}
		}
	})
}

func TestAttachmentReceiptSharedContentBilling(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name          string
		billable      []bool
		allowance     int64
		completion    bool
		customerOwned bool
		detailStore   bool
		wantLimit     string
	}{
		{name: "receipt first", billable: []bool{false, true, false}, allowance: 10000},
		{name: "content first", billable: []bool{true, false, true}, allowance: 10000},
		{name: "receipt only", billable: []bool{false, false}},
		{name: "detail store shared", billable: []bool{false, true, false}, allowance: 10000, detailStore: true},
		{name: "API refusal", billable: []bool{true}, allowance: 10000, wantLimit: "api_mutations"},
		{name: "quota refusal", billable: []bool{true}, wantLimit: "collaboration_bytes"},
		{name: "completion while exhausted", billable: []bool{true, false, true}, completion: true},
		{name: "customer owned", billable: []bool{false, true, false}, customerOwned: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			repository := projectRecordPostgres(t)
			plans := pilotHostedPlans()
			for i := range plans.Plans {
				plans.Plans[i].Allowances["collaboration_bytes"] = test.allowance
				plans.Plans[i].Allowances["api_mutations"] = 10000
				if test.wantLimit == "api_mutations" || test.completion {
					plans.Plans[i].Allowances["api_mutations"] = 0
				}
			}
			for _, service := range configuredPlanPostgresServices(t, plans, repository) {
				organization := service.database.hostedOrganization
				t.Run(string(organization), func(t *testing.T) {
					objects := &changeObjectFixture{}
					service.config.TenantRepository = repository
					service.config.Content = objects.call
					if test.detailStore {
						service.config.Content = nil
						service.config.RunDetails = &memoryRunDetails{}
					}
					ctx := service.contentContext(t.Context())
					ctx.Value(changeContentKey{}).(*changeContent).completion = test.completion
					var charged bool
					var previous attachment.ArchiveReference
					for _, billable := range test.billable {
						var ref attachment.ArchiveReference
						var err error
						if test.customerOwned {
							ref, _, err = attachment.PrepareArchive(string(organization), 1, strings.NewReader("shared attachment metadata"))
							if err != nil {
								t.Fatal(err)
							}
							ref.CustomerOwned, ref.BindingID = true, "binding_example"
							err = service.registerChangeContent(ctx, string(organization), ref, billable)
						} else {
							ref, err = prepareMeteredContent(ctx, string(organization), []byte("shared attachment metadata"), billable)
						}
						var limit *hostedLimitError
						if test.wantLimit != "" {
							if !errors.As(err, &limit) || limit.Resource != test.wantLimit {
								t.Fatalf("billable=%t limit=%v err=%v", billable, limit, err)
							}
						} else if err != nil {
							t.Fatal(err)
						}
						if previous.Key != "" && previous != ref {
							t.Fatalf("shared reference changed: before=%+v after=%+v", previous, ref)
						}
						previous = ref
						charged = charged || billable && !test.customerOwned && test.wantLimit == ""
						var want int64
						if charged {
							want = ref.CompressedSize
						}
						if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
							used, err := r.CollaborationBytes(t.Context())
							if err != nil {
								return err
							}
							if used != want {
								t.Fatalf("billable=%t usage=%d want=%d", billable, used, want)
							}
							object, err := r.ArchiveObjectByReference(t.Context(), ref)
							if test.wantLimit != "" {
								if !errors.Is(err, corestore.ErrNotFound) {
									t.Fatalf("refused registration persisted: object=%+v err=%v", object, err)
								}
								return nil
							}
							if err == nil && (object.OrganizationID != organization || object.CollaborationBytes != want || object.Reference != ref) {
								t.Fatalf("registered object=%+v want debit=%d", object, want)
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
					if test.wantLimit != "" {
						objects.mu.Lock()
						writes := objects.writes
						objects.mu.Unlock()
						if writes != 0 {
							t.Fatalf("quota refusal wrote %d objects", writes)
						}
						return
					}
					if err := service.registerChangeContent(ctx, string(organization), previous, charged || test.customerOwned); err != nil {
						t.Fatalf("durable registration replay: %v", err)
					}
					if err := service.registerChangeContent(t.Context(), string(organization)+"_foreign", previous, true); !errors.Is(err, attachment.ErrInvalid) {
						t.Fatalf("foreign organization registration=%v", err)
					}
					if err := repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						used, err := r.CollaborationBytes(t.Context())
						want := int64(0)
						if charged {
							want = previous.CompressedSize
						}
						if err != nil {
							return err
						}
						if used != want {
							t.Fatalf("durable replay usage=%d want=%d", used, want)
						}
						now := service.config.now()
						window := time.Unix(now.Unix()/plans.WindowSeconds*plans.WindowSeconds, 0).UTC()
						apiUsed, err := r.BillingUsage(t.Context(), window, "api_mutations")
						if err != nil && !errors.Is(err, corestore.ErrNotFound) {
							return err
						}
						if apiUsed != 0 {
							t.Fatalf("registration replay charged API mutations=%d", apiUsed)
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
				})
			}
		})
	}
}

func TestAttachmentReceiptBillingMigration(t *testing.T) {
	t.Parallel()
	objects := &changeObjectFixture{}
	service := newPostgresNativeService(t, Config{Content: objects.call, RunDetails: objects})
	parent := t
	for _, org := range []tracker.OrganizationID{"org_first", "org_first_second"} {
		t.Run(string(org), func(t *testing.T) {
			ctx := t.Context()
			repository := service.config.TenantRepository
			content := eventstore.NewContent(objects)
			var initial int64
			if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				initial, err = r.CollaborationBytes(ctx)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			var total int64
			var shared attachment.ArchiveReference
			var receipts []eventstore.Receipt
			for _, operation := range []string{"attachment.record example", "example.record", "attachment.record shared"} {
				raw := historyTestJSON(t, map[string]string{"operation": operation})
				ref, err := objects.StoreDetail(ctx, string(org), raw)
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := content.PrepareReceipt(ctx, eventstore.Receipt{OrganizationID: org, ActorID: "historical-operator", Operation: operation, CommandKey: operation, RequestHash: artifact.Digest([]byte(operation)), CreatedAt: service.config.now()}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if receipt.PayloadObjectKey != ref.Key {
					t.Fatal("command receipt did not share its registered content")
				}
				if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.LockArchiveAccounting(ctx); err != nil {
						return err
					}
					object, err := w.RegisterArchiveObject(ctx, artifactstore.ArchiveObject{OrganizationID: org, ID: "archive_" + ref.SHA256, Reference: ref, CreatedAt: receipt.CreatedAt})
					if err != nil {
						return err
					}
					charged, err := w.ChargeArchiveObject(ctx, object.ID, math.MaxInt64)
					if err != nil {
						return err
					}
					if !charged.Allowed {
						return errors.New("historical fixture archive debit refused")
					}
					_, err = w.PutReceipt(ctx, receipt)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				total += ref.CompressedSize
				receipts = append(receipts, receipt)
				if operation == "attachment.record shared" {
					shared = ref
					receipt.Operation, receipt.CommandKey = "example.shared", "shared"
					if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
						_, err := w.PutReceipt(ctx, receipt)
						return err
					}); err != nil {
						t.Fatal(err)
					}
					receipts = append(receipts, receipt)
				}
			}
			for _, stage := range []string{"registered history", "durable replay", "restart replay"} {
				if stage == "restart replay" {
					service = reopenCoordinatorService(parent, service, func(cfg *Config) { cfg.Hosted.OrganizationID = "org_startup" })
					repository = service.config.TenantRepository
				}
				if stage != "registered history" {
					for _, receipt := range receipts {
						ref := eventstore.Reference(receipt.PayloadObjectKey, receipt.PayloadSHA256, receipt.PayloadBytes, receipt.PayloadContentBytes)
						if err := service.registerChangeContent(ctx, string(org), ref, false); err != nil {
							t.Fatal(err)
						}
						if err := repository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
							_, err := w.PutReceipt(ctx, receipt)
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				if err := repository.ReadTenant(ctx, org, corestore.Serving, func(r tenantstore.Reader) error {
					for _, expected := range receipts {
						stored, err := r.Receipt(ctx, expected.ActorID, expected.Operation, expected.CommandKey)
						if err != nil {
							return err
						}
						if stored.PayloadObjectKey != expected.PayloadObjectKey || stored.PayloadSHA256 != expected.PayloadSHA256 || stored.PayloadBytes != expected.PayloadBytes || stored.PayloadContentBytes != expected.PayloadContentBytes {
							t.Fatalf("%s receipt content identity changed: operation=%s", stage, expected.Operation)
						}
					}
					object, err := r.ArchiveObjectByReference(ctx, shared)
					if err != nil {
						return err
					}
					if object.CollaborationBytes != shared.CompressedSize {
						t.Errorf("shared command content became unbillable: debit=%d want=%d", object.CollaborationBytes, shared.CompressedSize)
					}
					used, err := r.CollaborationBytes(ctx)
					if err != nil || used != initial+total {
						t.Fatalf("%s reclassified history: usage=%d want=%d err=%v", stage, used, initial+total, err)
					}
					var recounted int64
					if err := hostedPostgresAdmin(t, service).QueryRowContext(ctx, "SELECT COALESCE(sum(collaboration_bytes),0) FROM archive_objects WHERE organization_id=$1", org).Scan(&recounted); err != nil {
						return err
					}
					if used != recounted {
						t.Fatalf("recounted receipt usage=%d want=%d", used, recounted)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestChangeContentWithoutLegacyDatabase(t *testing.T) {
	t.Run("immutable content without legacy database", func(t *testing.T) {
		defer func() {
			if recovered := recover(); recovered != nil {
				t.Errorf("immutable content requires legacy database: %v", recovered)
			}
		}()
		contentService := &Service{config: Config{RunDetails: &memoryRunDetails{}}}
		payload := []byte("retained source metadata")
		ref, err := contentService.runDetailStore().StoreDetail(t.Context(), "org_first", payload)
		if err != nil {
			t.Fatal(err)
		}
		got, err := readChangeContent(contentService.contentContext(t.Context()), "org_first", ref)
		if err != nil || !bytes.Equal(got, payload) {
			t.Fatalf("immutable content = %q, %v", got, err)
		}
	})
	t.Run("typed payload key scope and preparation", func(t *testing.T) {
		repository, admin := projectRecordPostgresWithAdmin(t, 3)
		objects := &memoryRunDetails{}
		organizations := []tracker.OrganizationID{"org_first", "org_second"}
		runs := make(map[tracker.OrganizationID]tracker.NativeRunData)
		payloads := make(map[tracker.OrganizationID][]byte)
		references := make(map[tracker.OrganizationID]attachment.ArchiveReference)
		historyReferences := make(map[tracker.OrganizationID]attachment.ArchiveReference)
		unregistered := make(map[tracker.OrganizationID]attachment.ArchiveReference)
		for _, org := range organizations {
			run := tracker.NativeRunData{RunID: "run_" + string(org), AttemptID: "attempt_" + string(org), LeaseID: "lease_example", FencingToken: 1, Sequence: 1, Outcome: "succeeded", CompletionBody: string(org)}
			raw, err := json.Marshal(run)
			if err != nil {
				t.Fatal(err)
			}
			ref, err := objects.StoreDetail(t.Context(), string(org), raw)
			if err != nil {
				t.Fatal(err)
			}
			history, err := json.Marshal(tracker.CollaborationData{Run: &run})
			if err != nil {
				t.Fatal(err)
			}
			historyRef, err := objects.StoreDetail(t.Context(), string(org), history)
			if err != nil {
				t.Fatal(err)
			}
			missing, err := objects.StoreDetail(t.Context(), string(org), []byte("unregistered content"))
			if err != nil {
				t.Fatal(err)
			}
			runs[org], payloads[org], references[org], historyReferences[org], unregistered[org] = run, raw, ref, historyRef, missing
			if err := repository.WriteTenant(t.Context(), org, corestore.Serving, func(w tenantstore.Writer) error {
				if err := w.CreateAccessOrganization(t.Context(), accessstore.Organization{OrganizationID: org, Name: string(org), CreatedAt: time.Now()}); err != nil {
					return err
				}
				for _, ref := range []attachment.ArchiveReference{ref, historyRef} {
					if _, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: org, ID: newNativeID("archive"), Reference: ref, CreatedAt: time.Now()}); err != nil {
						return err
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		}
		objects.beforeRead = func(ctx context.Context) error {
			var active bool
			if err := admin.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND pid<>pg_backend_pid() AND backend_type='client backend' AND xact_start IS NOT NULL)`).Scan(&active); err != nil {
				return err
			}
			if active {
				return errors.New("payload object read overlaps a database transaction")
			}
			return nil
		}
		for index, org := range organizations {
			t.Run(string(org), func(t *testing.T) {
				service := &Service{config: Config{TenantRepository: repository, Hosted: &HostedConfig{OrganizationID: string(org)}, RunDetails: objects}}
				metadataReads := 0
				newContext := func() (context.Context, *changeContent) {
					ctx := service.contentContext(t.Context())
					owner := ctx.Value(changeContentKey{}).(*changeContent)
					read := owner.readReference
					owner.readReference = func(ctx context.Context, key string) (attachment.ArchiveReference, error) {
						metadataReads++
						return read(ctx, key)
					}
					return ctx, owner
				}
				ctx, owner := newContext()
				ref, raw := references[org], payloads[org]
				got, err := readStoredPayload(ctx, ref.Key)
				if err != nil || !bytes.Equal(got, raw) || metadataReads != 1 {
					t.Fatalf("typed payload read=%q metadata_reads=%d error=%v", got, metadataReads, err)
				}
				got[0] ^= 1
				beforeReads := objects.reads
				contentWriting(ctx, true)
				got, err = readStoredPayload(ctx, ref.Key)
				contentWriting(ctx, false)
				if err != nil || !bytes.Equal(got, raw) || metadataReads != 1 || objects.reads != beforeReads {
					t.Fatalf("cached write-phase read changed bytes or read storage: error=%v", err)
				}
				foreign := organizations[1-index]
				owner.mu.Lock()
				owner.organization = string(foreign)
				err = owner.rememberPayload(references[foreign], payloads[foreign])
				owner.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				for _, key := range []string{references[foreign].Key, ref.Key + "/extra", strings.Replace(ref.Key, "/blobs/", "/other/", 1)} {
					if _, err := readStoredPayload(ctx, key); !errors.Is(err, artifact.ErrIntegrity) || metadataReads != 1 || objects.reads != beforeReads {
						t.Fatalf("invalid/foreign key reached storage or cached foreign data: metadata_reads=%d error=%v", metadataReads, err)
					}
				}
				var run tracker.NativeRunData
				if err := decodeRunDetail(ctx, nil, ref.Key, &run); err != nil || !reflect.DeepEqual(run, runs[org]) {
					t.Fatalf("typed run decode=%+v error=%v", run, err)
				}
				var history tracker.CollaborationData
				if err := decodeRunHistory(ctx, nil, historyReferences[org].Key, &history); err != nil || history.Run == nil || !reflect.DeepEqual(*history.Run, runs[org]) {
					t.Fatalf("typed history decode=%+v error=%v", history.Run, err)
				}
				if err := decodeRunHistory(ctx, nil, "event:obsolete", &history); err == nil {
					t.Fatal("obsolete SQLite event token was accepted")
				}
				fresh, _ := newContext()
				isolated := isolateContentContext(fresh)
				got, err = readStoredPayload(isolated, ref.Key)
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatalf("isolated context lost typed reference capability: %v", err)
				}
				ctx, _ = newContext()
				beforeMetadata, beforeReads := metadataReads, objects.reads
				if _, err := readStoredPayload(ctx, unregistered[org].Key); !errors.Is(err, corestore.ErrNotFound) || !errors.Is(err, artifact.ErrIntegrity) || objects.reads != beforeReads || metadataReads != beforeMetadata+1 {
					t.Fatalf("unregistered payload reached object storage: error=%v", err)
				}
				ctx, _ = newContext()
				objects.mu.Lock()
				delete(objects.data, ref.Key)
				objects.mu.Unlock()
				_, missingErr := readStoredPayload(ctx, ref.Key)
				objects.mu.Lock()
				objects.data[ref.Key] = bytes.Clone(raw)
				objects.mu.Unlock()
				if !errors.Is(missingErr, artifact.ErrMissing) || !errors.Is(missingErr, artifact.ErrStorage) {
					t.Fatalf("missing payload became success or lost classification: %v", missingErr)
				}
				ctx, _ = newContext()
				objects.mu.Lock()
				objects.data[ref.Key] = bytes.Clone(raw[:len(raw)-1])
				objects.mu.Unlock()
				_, corruptErr := readStoredPayload(ctx, ref.Key)
				objects.mu.Lock()
				objects.data[ref.Key] = bytes.Clone(raw)
				objects.mu.Unlock()
				if !errors.Is(corruptErr, artifact.ErrIntegrity) {
					t.Fatalf("truncated payload was accepted: %v", corruptErr)
				}
				ctx, owner = newContext()
				beforeMetadata, beforeReads = metadataReads, objects.reads
				contentWriting(ctx, true)
				_, err = readStoredPayload(ctx, ref.Key)
				var hydration *claimHydration
				if !errors.As(err, &hydration) || metadataReads != beforeMetadata || objects.reads != beforeReads {
					t.Fatalf("uncached write-phase read bypassed preparation: %v", err)
				}
				contentWriting(ctx, false)
				if err := hydration.run(ctx, nil); err != nil {
					t.Fatal(err)
				}
				got, err = readStoredPayload(ctx, ref.Key)
				if err != nil || !bytes.Equal(got, raw) || metadataReads != beforeMetadata+1 || objects.reads != beforeReads+1 {
					t.Fatalf("prepared payload was not reused: error=%v", err)
				}
				owner.mu.Lock()
				invalid := owner.references[ref.Key]
				invalid.UncompressedSize = 0
				owner.references[ref.Key] = invalid
				owner.mu.Unlock()
				if _, err := readStoredPayload(ctx, ref.Key); !errors.Is(err, artifact.ErrIntegrity) {
					t.Fatalf("invalid cached reference was accepted: %v", err)
				}
				local := &Service{config: Config{TenantRepository: repository, RunDetails: objects}, localOrganization: string(org)}
				got, err = readStoredPayload(local.contentContext(t.Context()), ref.Key)
				if err != nil || !bytes.Equal(got, raw) {
					t.Fatalf("canonical local tenant payload read: %v", err)
				}
				transportContext, _ := newContext()
				encoded, compressed, err := attachment.PrepareArchive(string(org), 1, bytes.NewReader(raw))
				if err != nil || encoded != ref {
					t.Fatalf("conversation transport fixture reference differs: %v", err)
				}
				probing, calls := false, 0
				transport := conversationArchiveStore{Call: func(ctx context.Context, request entryauthority.ContentRequest) ([]byte, error) {
					calls++
					if request.Organization != string(org) || request.Reference != ref || request.Write {
						return nil, artifact.ErrIntegrity
					}
					if !probing {
						probing = true
						value, err := readStoredPayload(ctx, ref.Key)
						probing = false
						if err != nil || !bytes.Equal(value, raw) {
							return nil, errors.Join(artifact.ErrIntegrity, err)
						}
					}
					return bytes.Clone(compressed), nil
				}}
				got, err = transport.ReadContent(transportContext, string(org), ref)
				if err != nil || !bytes.Equal(got, raw) || calls == 0 {
					t.Fatalf("conversation context lost typed reference capability: %v", err)
				}
				beforeCalls := calls
				if _, err := transport.ReadContent(transportContext, string(foreign), references[foreign]); !errors.Is(err, artifact.ErrIntegrity) || calls != beforeCalls {
					t.Fatalf("conversation context retargeted a bound tenant: %v", err)
				}
				unscoped := &Service{config: Config{TenantRepository: repository, RunDetails: objects}}
				if _, err := readStoredPayload(unscoped.contentContext(t.Context()), ref.Key); !errors.Is(err, errContentStorageUnconfigured) {
					t.Fatalf("key selected an unbound tenant: %v", err)
				}
			})
		}
	})

	t.Run("operator mutation retains typed payload capability", func(t *testing.T) {
		objects := &memoryRunDetails{}
		service := newPostgresNativeService(t, Config{RunDetails: objects})
		f := newChangeFixture(t, service)
		item := f.create(t, "Operator payload read")
		if err := service.stopGitHubWebhookMaintenance(); err != nil {
			t.Fatal(err)
		}
		raw := []byte("operator retained payload")
		ref, err := objects.StoreDetail(t.Context(), string(f.project.OrganizationID), raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := service.config.TenantRepository.WriteTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
			_, err := w.RegisterArchiveObject(t.Context(), artifactstore.ArchiveObject{OrganizationID: f.project.OrganizationID, ID: newNativeID("archive"), Reference: ref, CreatedAt: service.config.now()})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		probing, probes := false, 0
		objects.beforeRead = func(ctx context.Context) error {
			if probing {
				return nil
			}
			probing = true
			defer func() { probing = false }()
			got, err := readStoredPayload(ctx, ref.Key)
			if err != nil {
				return err
			}
			if !bytes.Equal(got, raw) {
				return artifact.ErrIntegrity
			}
			probes++
			return nil
		}
		defer func() { objects.beforeRead = nil }()
		ctx := changeOperatorContext(t, service, f.token, string(f.project.OrganizationID))
		credential, _, err := service.authenticateTenantAPIHash(t.Context(), f.project.OrganizationID, apikey.HashToken(f.token), "", "")
		if err != nil {
			t.Fatal(err)
		}
		app := hubChangeApplication{service: service, scope: nativeScope{organization: f.project.OrganizationID, credential: credential}}
		result, err := app.MutateChange(ctx, operatortool.CreateChange, operatortool.ChangeArguments{ProjectID: string(f.project.ID), ItemID: string(item.WorkItemID), RequestID: "typed-content-capability", Title: "Operator content"})
		if err != nil || result.ChangeID == "" || probes == 0 {
			t.Fatalf("operator mutation lost its typed content capability: change=%q probes=%d error=%v", result.ChangeID, probes, err)
		}
		var change tracker.ChangeRequest
		if err := json.Unmarshal(result.Receipt, &change); err != nil || change.ID != result.ChangeID || change.Title != "Operator content" {
			t.Fatalf("operator mutation receipt changed: %+v error=%v", change, err)
		}
	})

	t.Run("historical source and full review continuation", func(t *testing.T) {
		ctx := t.Context()
		f := newChangeFixture(t, nil)
		org := f.project.OrganizationID
		input := changeTestInput()
		bundle := changeTestSource(t, &input)
		response := performHubAPIRequest(t, f.service, http.MethodPost, f.path+"/versions", f.token, tracker.PublishChangeVersion{
			Mutation: tracker.Mutation{IdempotencyKey: "historical"}, ChangeVersionInput: input, SourceBundle: bundle,
		})
		requireNativeStatus(t, response, http.StatusOK)
		var historical tracker.ChangeVersion
		decodeHubResponse(t, response, &historical)
		current := f.publish(t, "current", historical.ID)
		if current.ID == historical.ID {
			t.Fatal("source fixture did not advance the current version")
		}
		response = performHubAPIRequest(t, f.service, http.MethodGet, f.path+"/versions/"+historical.ID+"/source", f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		if !bytes.Equal(response.Body.Bytes(), bundle) {
			t.Fatal("retained historical source bytes changed")
		}

		review := tracker.ChangeReview{ID: newNativeID("review"), VersionID: historical.ID, Decision: "commented",
			Actor: tracker.Actor{Kind: "human", PrincipalID: "reviewer"}, CreatedAt: f.service.config.now().UTC().Truncate(time.Microsecond),
			Validator: &gate.ValidatorResult{VersionID: historical.ID, Repository: historical.Repository, BaseSHA: historical.BaseSHA,
				HeadSHA: historical.HeadSHA, Verdict: "wait", NotVerified: []string{"Unresolved evidence question"},
				Commands: []gate.CommandResult{{Command: "make check-land", Output: strings.Repeat("full command output\n", 11000)[:200<<10],
					HeadSHA: historical.HeadSHA, TreeSHA: historical.BaseSHA, ExitCode: 1, DurationNS: 123}}},
		}
		raw, err := json.Marshal(review)
		if err != nil {
			t.Fatal(err)
		}
		content := eventstore.NewContent(f.service.runDetailStore())
		row, err := content.PrepareEvidence(ctx, eventstore.Evidence{OrganizationID: org, ChangeID: f.change.ID, Kind: "review"}, raw)
		if err != nil {
			t.Fatal(err)
		}
		var record eventstore.Evidence
		if err := f.service.config.TenantRepository.WriteTenant(ctx, org, corestore.Serving, func(w tenantstore.Writer) error {
			var err error
			record, err = w.AppendEvidence(ctx, row)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		path := f.base + "/work-items/" + string(f.issue.WorkItemID) + "?view=recovery&limit=1"
		var recovered []byte
		var cursor string
		for len(recovered) < len(raw) {
			request := path
			if cursor != "" {
				request += "&cursor=" + url.QueryEscape(cursor)
			}
			response := performHubAPIRequest(t, f.service, http.MethodGet, request, f.token, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var recovery tracker.NativeRecovery
			decodeHubResponse(t, response, &recovery)
			page := recovery.Continuation
			if page == nil || page.Section != "reviews" || page.VersionID != historical.ID || page.RecordID != strconv.FormatInt(record.Sequence, 10) ||
				page.Offset != int64(len(recovered)) || page.TotalBytes != int64(len(raw)) || len(page.Data) == 0 || len(page.Data) > 64<<10 || page.NextCursor == "" {
				t.Fatalf("wrong retained review fragment at %d", len(recovered))
			}
			recovered = append(recovered, page.Data...)
			cursor = page.NextCursor
		}
		var result tracker.ChangeReview
		if err := json.Unmarshal(recovered, &result); err != nil {
			t.Fatal(err)
		}
		if result.ID != review.ID || result.VersionID != historical.ID || !reflect.DeepEqual(result.Validator, review.Validator) {
			t.Fatal("recovery lost full review output or exact version")
		}
	})
}
