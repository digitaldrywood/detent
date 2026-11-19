package coreimport_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/coreimport"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubapi"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/reports"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/policy"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestChangeLandingColumnsPreserveHistoricalReads(t *testing.T) {
	t.Parallel()
	at := time.Now().UTC().Truncate(time.Second).Add(-5 * time.Hour)
	var clock atomic.Pointer[time.Time]
	clock.Store(new(at.Add(-2 * time.Hour)))
	cfg := hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider())
	cfg.Clock = func() time.Time { return *clock.Load() }
	h := hubfixture.OpenHosted(t, cfg)
	repository, objects := h.Repository, h.Config.RunDetails
	project := hubtest.Project(t, repository, h.Primary, hubtest.ProjectOptions{Name: "Historical landings"})
	descriptor := hubfixture.Policy()
	pause := errors.New("historical landing import paused")
	interrupted := &landingImportPauseDetails{DetailStore: objects, changeID: "historical_change_b", pause: pause}
	fixture := newCoreNeutralFixture(t, h.Primary, "change_requests", "change_issue_links", "change_versions", "change_evidence", "change_sources", "change_landing_receipts", "quality_landings", "change_viewed_files", "policy_revisions", "change_command_outputs", "change_finding_attributions", "change_content_objects", "attachments", "native_pr_publications", "release_deliveries")
	fixture.Add("policy_revisions", map[string]any{"scope": string(h.Primary) + "/" + string(project.ID), "policy_id": descriptor.ID, "metadata_json": string(historyTestJSON(t, descriptor))})
	var expected []reports.NativeAnalyticsLanding
	for i, suffix := range []string{"a", "b"} {
		item := hubtest.Issue(t, repository, objects, project, hubtest.IssueOptions{Title: "Historical item " + suffix, Now: at.Add(-2 * time.Hour)}).Issue
		clock.Store(new(at.Add(-2 * time.Minute)))
		attempt := historyRepairAttempt(t, hubfixture.Change{Native: hubfixture.Native{Hub: h.Hub, Project: project}, Issue: item}, nil)
		id := "historical_change_" + suffix
		actor := tracker.Actor{Kind: "human", PrincipalID: "operator"}
		version := tracker.ChangeVersion{ID: "historical_version_" + suffix, ChangeID: id, Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), Repository: "owner/repository", PolicyID: descriptor.ID, AttemptID: attempt.ID}, Actor: actor, CreatedAt: at.Add(-time.Minute)}
		landing := tracker.ChangeLanding{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: strings.Repeat(strconv.Itoa(i+1), 40), BaseRef: "develop", LandedAt: at, Actor: actor}
		change := tracker.ChangeRequest{ID: id, OrganizationID: h.Primary, ProjectID: project.ID, WorkItemID: item.WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{item.WorkItemID}, Title: "Historical change", CurrentVersion: version.ID, Revision: 1, CreatedAt: at.Add(-time.Hour), UpdatedAt: at}
		if i == 0 {
			change.Landed = &landing
		}
		fixture.Add("change_requests", map[string]any{"id": id, "organization_id": h.Primary, "project_id": project.ID, "work_item_id": item.WorkItemID, "record_json": string(historyTestJSON(t, change)), "created_at": nil, "landed_at": nil, "landed_version_id": nil, "landed_head_sha": nil, "landed_merge_sha": nil, "landed_base_ref": nil})
		fixture.Add("change_issue_links", map[string]any{"change_id": id, "organization_id": h.Primary, "project_id": project.ID, "work_item_id": item.WorkItemID})
		fixture.Add("change_versions", map[string]any{"id": version.ID, "change_id": id, "number": int64(1), "policy_revision_id": descriptor.ID, "record_json": string(historyTestJSON(t, version)), "head_sha": nil, "repository": version.Repository})
		receipt := tracker.NativeLandingReceipt{VersionID: version.ID, HeadSHA: version.HeadSHA, MergeSHA: landing.MergeSHA, BaseRef: landing.BaseRef, Landed: true, ObservedAt: at}
		fixture.Add("change_landing_receipts", map[string]any{"attempt_id": attempt.ID, "version_id": version.ID, "record_json": string(historyTestJSON(t, receipt))})
		if i == 1 {
			fixture.Add("quality_landings", map[string]any{"version_id": version.ID, "record_json": string(historyTestJSON(t, landing)), "landed_at": nil})
		}
		expected = append(expected, reports.NativeAnalyticsLanding{ChangeID: id, WorkItemID: item.WorkItemID, Landing: landing})
	}
	clock.Store(new(at.Add(5 * time.Hour)))
	owner := h.User(t, "owner", "owner", "owner@example.test", "", "")
	h.Grant(t, owner, project.ID, true, false)
	read := func(from, to time.Time) reports.NativeAnalyticsProject {
		t.Helper()
		return readHistoricalLandingReport(t, h, owner, project.ID, from, to)
	}
	source := coreimport.CoreChangeImportSource{BundlePath: fixture.Write(), OrganizationID: h.Primary, SourceObjects: objects, Objects: interrupted}
	if _, err := coreimport.ImportCoreChanges(t.Context(), repository, source); !errors.Is(err, pause) {
		t.Fatalf("interrupted historical import error=%v want=%v", err, pause)
	}
	from, to := at.Add(-time.Hour), at.Add(time.Hour)
	assertHistoricalLandingReport(t, read(from, to), expected[:1])
	interrupted.pause = nil
	if _, err := coreimport.ImportCoreChanges(t.Context(), repository, source); err != nil {
		t.Fatal(err)
	}
	assertHistoricalLandingReport(t, read(from, to), expected)
	if _, err := coreimport.ImportCoreChanges(t.Context(), repository, source); err != nil {
		t.Fatal(err)
	}
	assertHistoricalLandingReport(t, read(from, to), expected)

	unproven := seedUnprovenHistoricalLanding(t, h.Hub, descriptor, at)
	for _, test := range []struct {
		name, repository, commit, branch, change string
		project                                  tracker.NativeProject
	}{
		{"matching first", "owner/repository", expected[0].Landing.MergeSHA, "develop", expected[0].ChangeID, project},
		{"interrupted imported landing has no provenance", "owner/repository", strings.Repeat("4", 40), "develop", "", unproven},
		{"interrupted imported landing has no history provenance", "owner/repository", strings.Repeat("4", 40), "", "", unproven},
		{"different commit", "owner/repository", strings.Repeat("3", 40), "develop", "", project},
		{"different branch", "owner/repository", expected[0].Landing.MergeSHA, "feature", "", project},
		{"different repository", "other/repository", expected[0].Landing.MergeSHA, "develop", "", project},
		{"later matching landing", "owner/repository", expected[1].Landing.MergeSHA, "develop", expected[1].ChangeID, project},
	} {
		t.Run(test.name, func(t *testing.T) {
			var got eventstore.PolicyLanding
			err := repository.ReadTenant(t.Context(), test.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
				var err error
				got, err = r.ProjectPolicyLanding(t.Context(), test.project.ID, test.repository, test.commit, test.branch)
				return err
			})
			if test.change == "" {
				if !errors.Is(err, corestore.ErrNotFound) || got.ChangeID != "" {
					t.Fatalf("landing=%+v want no proven Change, error=%v", got, err)
				}
				return
			}
			if err != nil || got.ChangeID != test.change {
				t.Fatalf("landing=%+v want Change=%q error=%v", got, test.change, err)
			}
		})
	}

	var reopenEvents []string
	for _, row := range expected {
		reopenEvents = append(reopenEvents, appendHistoricalLandingReopen(t, h.Hub, project, row.WorkItemID, at.Add(3*time.Hour)))
	}
	for _, resume := range []bool{false, true} {
		if resume {
			if _, err := coreimport.ImportCoreChanges(t.Context(), repository, source); err != nil {
				t.Fatal(err)
			}
		}
		report := read(at.Add(2*time.Hour), at.Add(4*time.Hour))
		reopened := report.Quality.Occurrences.Items
		slices.SortFunc(reopened, func(a, b reports.AnalyticsQualityEscape) int { return strings.Compare(a.ChangeID, b.ChangeID) })
		if !slices.EqualFunc(reopened, expected, func(got reports.AnalyticsQualityEscape, want reports.NativeAnalyticsLanding) bool {
			return got.ChangeID == want.ChangeID && got.VersionID == want.Landing.VersionID && got.MergeSHA == want.Landing.MergeSHA && got.WorkItemID == string(want.WorkItemID) && got.Kind == "reopened"
		}) {
			t.Fatalf("reopened historical landings=%+v want=%+v", reopened, expected)
		}
		for i, event := range reopenEvents {
			if reopened[i].OccurrenceID != event {
				t.Fatalf("reopen occurrence=%s want event=%s", reopened[i].OccurrenceID, event)
			}
		}
	}
}

func readHistoricalLandingReport(t *testing.T, h *hubfixture.Hosted, owner hubfixture.HostedUser, project tracker.ProjectID, from, to time.Time) reports.NativeAnalyticsProject {
	t.Helper()
	args := map[string]any{"project_id": project, "from": from.Format(time.RFC3339), "to": to.Format(time.RFC3339), "bucket": "1h"}
	call := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{
		"name": operatortool.Reports, "arguments": args, "_meta": json.RawMessage(hubfixture.MCPProtocolMeta),
	}}
	request := httptest.NewRequestWithContext(t.Context(), http.MethodPost, hubfixture.MCPPath(h.Primary), strings.NewReader(string(historyTestJSON(t, call))))
	request.AddCookie(&http.Cookie{Name: hubfixture.HostedCookie, Value: owner.Token})
	request.Header.Set("X-CSRF-Token", hubfixture.HostedCSRF(owner.Token))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Mcp-Protocol-Version", hubfixture.MCPProtocolVersion)
	request.Header.Set("Mcp-Method", "tools/call")
	request.Header.Set("Mcp-Name", operatortool.Reports)
	response := httptest.NewRecorder()
	h.Handler().ServeHTTP(response, request)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var reply hubfixture.MCPReply
	hubfixture.Decode(t, response, &reply)
	var report reports.NativeAnalyticsReport
	if err := json.Unmarshal(hubfixture.MCPData(t, reply, false), &report); err != nil {
		t.Fatal(err)
	}
	for _, data := range report.Projects {
		if data.ProjectID == string(project) {
			return data
		}
	}
	t.Fatalf("analytics omitted project %s", project)
	return reports.NativeAnalyticsProject{}
}

func assertHistoricalLandingReport(t *testing.T, report reports.NativeAnalyticsProject, expected []reports.NativeAnalyticsLanding) {
	t.Helper()
	rows := report.Landings.Items
	slices.SortFunc(rows, func(a, b reports.NativeAnalyticsLanding) int { return strings.Compare(a.ChangeID, b.ChangeID) })
	if !slices.EqualFunc(rows, expected, func(got, want reports.NativeAnalyticsLanding) bool {
		return got.ChangeID == want.ChangeID && got.WorkItemID == want.WorkItemID && got.Landing.VersionID == want.Landing.VersionID && got.Landing.MergeSHA == want.Landing.MergeSHA && got.Landing.LandedAt.Equal(want.Landing.LandedAt)
	}) {
		t.Fatalf("historical landings=%+v want=%+v", rows, expected)
	}
}

func seedUnprovenHistoricalLanding(t *testing.T, h *hubfixture.Hub, descriptor policy.Descriptor, at time.Time) tracker.NativeProject {
	t.Helper()
	repository, objects := h.Repository, h.Config.RunDetails
	organization := h.Primary + "_historical_unproven"
	h.EnsureOrganization(t, organization)
	project := hubtest.Project(t, repository, organization, hubtest.ProjectOptions{Name: "Unproven historical landings"})
	item := hubtest.Issue(t, repository, objects, project, hubtest.IssueOptions{Title: "Unproven historical item", Now: at.Add(-2 * time.Hour)}).Issue
	unprovenVersion := tracker.ChangeVersion{ID: "unproven_historical_version", ChangeID: "unproven_historical_change", Number: 1, ChangeVersionInput: tracker.ChangeVersionInput{HeadSHA: strings.Repeat("a", 40), Repository: "owner/repository", PolicyID: descriptor.ID}, Policy: descriptor, Actor: tracker.Actor{Kind: "human", PrincipalID: "operator"}, CreatedAt: at.Add(-time.Minute)}
	unprovenLanding := tracker.ChangeLanding{VersionID: unprovenVersion.ID, HeadSHA: unprovenVersion.HeadSHA, MergeSHA: strings.Repeat("4", 40), BaseRef: "develop", LandedAt: at, Actor: unprovenVersion.Actor}
	unprovenChange := tracker.ChangeRequest{ID: unprovenVersion.ChangeID, OrganizationID: project.OrganizationID, ProjectID: project.ID, WorkItemID: item.WorkItemID, LinkedIssues: []tracker.NativeWorkItemID{item.WorkItemID}, Revision: 1, Title: "Unproven imported landing", CurrentVersion: unprovenVersion.ID, Landed: &unprovenLanding, CreatedAt: at.Add(-time.Hour), UpdatedAt: at}
	unprovenContent := eventstore.NewContent(objects)
	unprovenInitial := unprovenChange
	unprovenInitial.CurrentVersion = ""
	unprovenChangeRecord, err := unprovenContent.PrepareChange(t.Context(), project.OrganizationID, historyTestJSON(t, unprovenInitial))
	if err != nil {
		t.Fatal(err)
	}
	unprovenVersionRecord, err := unprovenContent.PrepareVersion(t.Context(), project.OrganizationID, historyTestJSON(t, unprovenVersion))
	if err != nil {
		t.Fatal(err)
	}
	unprovenVersionRecord.LandingHistoryKnown = false
	if err := repository.WriteRecords(t.Context(), project.OrganizationID, corestore.Background, func(w eventstore.Writer) error {
		if _, err := w.CreateChange(t.Context(), unprovenChangeRecord); err != nil {
			return err
		}
		if err := w.LinkIssue(t.Context(), eventstore.Link{OrganizationID: project.OrganizationID, ProjectID: project.ID, WorkItemID: item.WorkItemID, ChangeID: unprovenChange.ID}); err != nil {
			return err
		}
		_, err := w.CreateVersion(t.Context(), unprovenVersionRecord)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
		change, err := r.Change(t.Context(), unprovenChange.ID)
		if err != nil {
			return err
		}
		version, err := r.Version(t.Context(), unprovenChange.ID, unprovenVersion.ID)
		if err != nil {
			return err
		}
		if change.LandedVersionID == nil || *change.LandedVersionID != unprovenVersion.ID || version.LandingHistoryKnown || version.FrozenLanding != nil {
			t.Fatalf("fixture lost actual unproven import state: change=%+v version=%+v", change, version)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return project
}

func appendHistoricalLandingReopen(t *testing.T, h *hubfixture.Hub, project tracker.NativeProject, item tracker.NativeWorkItemID, at time.Time) string {
	t.Helper()
	event := tracker.CollaborationEvent{ID: hubapi.NewID("event"), OrganizationID: project.OrganizationID, ProjectID: project.ID, AggregateType: "issue", AggregateID: item, Type: "workflow.transitioned", SchemaVersion: 1, Actor: tracker.Actor{Kind: "human", PrincipalID: "example"}, RecordedAt: at, Data: tracker.CollaborationData{FromState: "Done", ToState: "Rework"}}
	row, err := eventstore.NewContent(h.Config.RunDetails).PrepareEvent(t.Context(), project.OrganizationID, historyTestJSON(t, event))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Repository.WriteTenant(t.Context(), project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		_, err := w.AppendEvent(t.Context(), row)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return event.ID
}

type landingImportPauseDetails struct {
	attachment.DetailStore
	changeID string
	pause    error
}

func (s *landingImportPauseDetails) StoreDetail(ctx context.Context, organization string, raw []byte) (attachment.ArchiveReference, error) {
	var value tracker.ChangeRequest
	if s.pause != nil && json.Unmarshal(raw, &value) == nil && value.ID == s.changeID && value.Revision > 0 {
		return attachment.ArchiveReference{}, s.pause
	}
	return s.DetailStore.StoreDetail(ctx, organization, raw)
}
