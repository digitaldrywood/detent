package hubserver

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/changerequest"
	"github.com/digitaldrywood/detent/internal/gate"
	hubapi "github.com/digitaldrywood/detent/internal/hubclient/api"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/leasestore"
	"github.com/digitaldrywood/detent/internal/hubserver/policystore"
	"github.com/digitaldrywood/detent/internal/hubserver/postgres"
	"github.com/digitaldrywood/detent/internal/hubserver/runnerstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/runnerauth"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type changeReadInterceptionStore struct {
	attachment.DetailStore
	key       string
	afterRead func(context.Context) error
}

func (s *changeReadInterceptionStore) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	raw, err := s.DetailStore.ReadDetail(ctx, organization, ref)
	if err != nil {
		return nil, err
	}
	if ref.Key == s.key && s.afterRead != nil {
		callback := s.afterRead
		s.afterRead = nil
		if err := callback(ctx); err != nil {
			return nil, err
		}
	}
	return raw, nil
}

func TestPostgresBoundedChangeReadRechecksRoleAfterHydration(t *testing.T) {
	repository, f, owner, _ := newChangePolicyPostgresFixtureWithAdmin(t, 3)
	if err := f.service.stopGitHubWebhookMaintenance(); err != nil {
		t.Fatal(err)
	}
	items := f.base + "/work-items"
	created := f.request(t, owner, http.MethodPost, items, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "bounded-role-issue"}, Title: "Example private issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, created, http.StatusOK)
	var issue tracker.NativeIssue
	decodeHubResponse(t, created, &issue)
	created = f.request(t, owner, http.MethodPost, items, tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "bounded-secondary-issue"}, Title: "Example secondary issue", Body: workItemRecordBody, State: "Todo"})
	requireNativeStatus(t, created, http.StatusOK)
	var secondary tracker.NativeIssue
	decodeHubResponse(t, created, &secondary)
	changes := items + "/" + string(issue.WorkItemID) + "/changes"
	created = f.request(t, owner, http.MethodPost, changes, tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: "bounded-role-change"}, Title: "Example private Change", LinkedIssues: []tracker.NativeWorkItemID{secondary.WorkItemID}})
	requireNativeStatus(t, created, http.StatusOK)
	var change tracker.ChangeRequest
	decodeHubResponse(t, created, &change)
	details := f.details
	key := seedChangeReadCredential(t, repository, f.project)
	var keyRevoked bool
	details.beforeRead = func(ctx context.Context) error {
		details.beforeRead = nil
		keyRevoked = true
		return repository.WriteTenant(ctx, "org_security", corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessCredential(ctx, key.ID, time.Now().UTC()) })
	}
	t.Cleanup(func() { details.beforeRead = nil })
	keyRead := performHubAPIRequest(t, f.service, http.MethodGet, changes+"/"+change.ID+"?view=bounded", key.Token, nil)
	if !keyRevoked {
		t.Fatal("key was not revoked during source hydration")
	}
	if keyRead.Code == http.StatusOK || strings.Contains(keyRead.Body.String(), "Example private Change") {
		t.Fatalf("revoked key received Change data: %d %s", keyRead.Code, keyRead.Body.String())
	}
	var changed bool
	details.beforeRead = func(ctx context.Context) error {
		details.beforeRead = nil
		changed = true
		return repository.WriteTenant(ctx, "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			member, err := w.AccessMember(ctx, owner.identity.Subject)
			if err != nil {
				return err
			}
			member.Role = "reporter"
			return w.UpdateAccessMember(ctx, member)
		})
	}
	t.Cleanup(func() { details.beforeRead = nil })
	response := f.request(t, owner, http.MethodGet, changes+"/"+change.ID+"?view=bounded", nil)
	if !changed {
		t.Fatal("role did not change during source hydration")
	}
	if response.Code == http.StatusOK || strings.Contains(response.Body.String(), "Example private Change") {
		t.Fatalf("downgraded member received Change data: %d %s", response.Code, response.Body.String())
	}
	response = f.request(t, owner, http.MethodGet, changes+"/"+change.ID+"?view=bounded", nil)
	requireNativeStatus(t, response, http.StatusOK)
	var oldVersion eventstore.CollaborationVersion
	if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
		var err error
		oldVersion, err = r.CollaborationVersion(t.Context(), string(secondary.WorkItemID), int64(secondary.Revision))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	transferred := secondary
	transferred.Revision++
	transferred.Actor.PrincipalID = "other_principal"
	raw, err := json.Marshal(transferred)
	if err != nil {
		t.Fatal(err)
	}
	newVersion, err := eventstore.NewContent(details).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: "org_security", ProjectID: f.project, WorkItemID: secondary.WorkItemID, RecordID: string(secondary.WorkItemID), Revision: int64(transferred.Revision)}, raw)
	if err != nil {
		t.Fatal(err)
	}
	intercept := &changeReadInterceptionStore{DetailStore: details, key: oldVersion.PayloadObjectKey}
	intercept.afterRead = func(ctx context.Context) error {
		return repository.WriteTenant(ctx, "org_security", corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.LockWorkItemProject(ctx, f.project); err != nil {
				return err
			}
			row, err := w.WorkItem(ctx, f.project, secondary.WorkItemID)
			if err != nil {
				return err
			}
			row.ActorPrincipalID = &transferred.Actor.PrincipalID
			if _, err := w.UpdateWorkItem(ctx, row); err != nil {
				return err
			}
			_, err = w.CreateCollaborationVersion(ctx, newVersion)
			return err
		})
	}
	f.service.config.RunDetails = intercept
	t.Cleanup(func() { f.service.config.RunDetails = details })
	response = f.request(t, owner, http.MethodGet, changes+"/"+change.ID+"?view=bounded", nil)
	if intercept.afterRead != nil {
		t.Fatal("secondary ownership did not change during hydration")
	}
	if response.Code == http.StatusOK || strings.Contains(response.Body.String(), string(secondary.WorkItemID)) {
		t.Fatalf("reporter received a no-longer-visible linked issue: %d %s", response.Code, response.Body.String())
	}
	response = f.request(t, owner, http.MethodGet, changes+"/"+change.ID+"?view=bounded", nil)
	requireNativeStatus(t, response, http.StatusOK)
	if strings.Contains(response.Body.String(), string(secondary.WorkItemID)) {
		t.Fatal("reporter received an inaccessible linked issue on reread")
	}
	foreignPath := items + "/" + string(secondary.WorkItemID) + "/changes/" + change.ID + "?view=bounded"
	foreignRead := f.request(t, owner, http.MethodGet, foreignPath, nil)
	if foreignRead.Code != http.StatusNotFound || strings.Contains(foreignRead.Body.String(), "Example private Change") {
		t.Fatalf("inaccessible linked issue exposed the Change: %d %s", foreignRead.Code, foreignRead.Body.String())
	}
}

func TestPostgresBoundedChangeRuntimeRecovery(t *testing.T) {
	repository, f, owner, admin := newChangePolicyPostgresFixtureWithAdmin(t, 3)
	server := httptest.NewServer(f.service.Handler())
	t.Cleanup(server.Close)
	key := seedChangeReadCredential(t, repository, f.project)
	originalDB, originalReader := f.service.database.db, f.service.database.reader
	f.service.database.db, f.service.database.reader = nil, nil
	t.Cleanup(func() { f.service.database.db, f.service.database.reader = originalDB, originalReader })
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	client, err := hubapi.New(hubapi.Config{URL: server.URL, TokenSource: func() string { return key.Token }, HTTPClient: &http.Client{Transport: transport}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := client.Native("org_security", f.project)
	if err != nil {
		t.Fatal(err)
	}
	var firstChange string
	for _, key := range []string{"first", "second"} {
		t.Run(key, func(t *testing.T) {
			response := f.request(t, owner, http.MethodPost, f.base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Example issue", Body: workItemRecordBody, State: "Todo"})
			requireNativeStatus(t, response, http.StatusOK)
			var issue tracker.NativeIssue
			decodeHubResponse(t, response, &issue)
			response = f.request(t, owner, http.MethodPost, f.base+"/work-items/"+string(issue.WorkItemID)+"/changes", tracker.CreateChange{Mutation: tracker.Mutation{IdempotencyKey: key}, Title: "Example Change"})
			requireNativeStatus(t, response, http.StatusOK)
			var change tracker.ChangeRequest
			decodeHubResponse(t, response, &change)
			draft, err := native.RuntimeChange(t.Context(), issue.WorkItemID, change.ID)
			if err != nil || draft.Summary.Status != "draft" || len(draft.Versions) != 0 {
				t.Fatalf("draft recovery = %+v, %v", draft, err)
			}
			content := eventstore.NewContent(f.service.runDetailStore())
			actor := tracker.Actor{Kind: "human", PrincipalID: owner.identity.Subject}
			now := f.service.config.now().UTC()
			var latest tracker.ChangeVersion
			for number := int64(1); number <= 2; number++ {
				input := changeTestInput()
				input.AttemptID = "attempt_" + key

				bundle := changeTestSource(t, &input)
				source, err := content.PrepareSourceBundle(t.Context(), "org_security", bundle)
				if err != nil {
					t.Fatal(err)
				}
				latest = tracker.ChangeVersion{ID: fmt.Sprintf("version_%s_%d", key, number), ChangeID: change.ID, Number: number, ChangeVersionInput: input, Policy: hubTestPolicy(), ReviewPolicy: defaultChangeReviewPolicy(hubTestPolicy()), Actor: actor, CreatedAt: now}
				latest.Checks = []tracker.ChangeCheckExpectation{{CheckRunID: "check_" + key, ChangeCheckSpec: tracker.ChangeCheckSpec{MaxAgeSeconds: 3600}}}
				latest.Policy.Gates.GitHubPullRequest = true
				latest.Policy = latest.Policy.WithID()
				latest.PolicyID = latest.Policy.ID
				latest.ReviewPolicy.PolicyID = latest.PolicyID
				latest.ReviewPolicy.RequireReview = true
				latest.ReviewPolicy.ID = changerequest.PolicyID(latest.ReviewPolicy)
				raw, err := json.Marshal(latest)
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareVersion(t.Context(), "org_security", raw)
				if err != nil {
					t.Fatal(err)
				}
				row.SourceObjectKey, row.SourceSHA256, row.SourceBytes, row.SourceContentBytes = source.Key, source.SHA256, source.CompressedSize, source.UncompressedSize
				if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.CreateVersion(t.Context(), row); return err }); err != nil {
					t.Fatal(err)
				}
			}
			if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				row, err := w.Change(t.Context(), change.ID)
				if err != nil {
					return err
				}
				row.CurrentVersionID = new(latest.ID)
				_, err = w.UpdateChange(t.Context(), row)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			legacyEvidence := false
			appendEvidence := func(kind string, value any) {
				t.Helper()
				raw, err := json.Marshal(value)
				if err != nil {
					t.Fatal(err)
				}
				row, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: "org_security", ChangeID: change.ID, Kind: kind}, raw)
				if err != nil {
					t.Fatal(err)
				}
				if legacyEvidence && kind == "review" {
					if row.ReviewKind != "validator" {
						row.ReviewKind = ""
					}
					row.ReviewLanding = false
					row.ReviewSessionKnown, row.ReviewSessionID, row.ReviewSessionMachine = false, 0, ""
				}
				if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), row); return err }); err != nil {
					t.Fatal(err)
				}
			}
			appendEvidence("review", tracker.ChangeReview{ID: "old_validator_" + key, VersionID: "version_" + key + "_1", Decision: "approved", Validator: &gate.ValidatorResult{Verdict: "pass"}, Actor: actor, CreatedAt: now})
			response = f.request(t, owner, http.MethodGet, f.base+"/work-items/"+string(issue.WorkItemID)+"/changes/"+change.ID+"?view=bounded", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var oldMachine operatortool.ChangeResult
			decodeHubResponse(t, response, &oldMachine)
			if oldMachine.Detail.Summary.NativeReview != "pending" {
				t.Fatalf("old machine approval became human approval: %+v", oldMachine.Detail.Summary)
			}
			decision, conclusion, status := "approved", "success", "reviewed"
			if key == "second" {
				decision, conclusion, status = "changes_requested", "failure", "needs_evidence"
			}
			body := "retained review"
			if key == "second" {
				body = strings.Repeat("retained review ", operatortool.MaxResultBytes/4)
			}
			appendEvidence("review", tracker.ChangeReview{ID: "review_" + key, VersionID: latest.ID, Decision: decision, Body: body, Actor: actor, CreatedAt: now})
			appendEvidence("check", tracker.ChangeCheck{VersionID: latest.ID, ChangeCheckResult: tracker.ChangeCheckResult{CheckRunID: "check_" + key, Conclusion: conclusion, CompletedAt: now}, Actor: actor, ReceivedAt: now})
			discussionCount := 105
			if key == "second" {
				discussionCount = 1005
			}
			for i := 0; i < discussionCount; i++ {
				appendEvidence("discussion", tracker.ChangeDiscussion{ID: fmt.Sprintf("discussion_%s_%d", key, i), Body: "Example discussion", Actor: actor, CreatedAt: now})
			}
			run := tracker.NativeRunData{AttemptID: "attempt_" + key, RunID: "run_" + key, Sequence: 1, PolicyID: hubTestPolicy().ID}
			reference := tracker.NativeChangeReference{ChangeID: change.ID, VersionID: latest.ID, HeadSHA: latest.HeadSHA}
			run.Handoff = &tracker.NativeCheckpoint{Change: &reference, HeadSHA: latest.HeadSHA, ExternalEffect: "git_push", EffectState: "confirmed", EffectID: fmt.Sprintf("effect_%032x", len(key))}
			if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				item, err := w.WorkItem(t.Context(), f.project, issue.WorkItemID)
				if err != nil {
					return err
				}
				machine := "machine_" + key
				if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_security", ID: machine, Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
					return err
				}
				lease, err := w.Claim(t.Context(), leasestore.Claim{IssueID: item.ID, ExpectedRevision: item.Revision, MachineID: machine, SessionID: "session_" + key, LeaseID: "lease_" + key, TTL: time.Hour})
				if err != nil {
					return err
				}
				if err := w.BindLeasePolicy(t.Context(), policystore.LeasePolicy{OrganizationID: "org_security", ProjectID: f.project, LeaseID: tracker.LeaseID(lease.LeaseID), PolicyID: run.PolicyID, BoundAt: now}); err != nil {
					return err
				}
				run.LeaseID = tracker.LeaseID(lease.LeaseID)
				run.FencingToken = tracker.FencingToken(lease.FencingToken)
				raw, err := json.Marshal(tracker.CollaborationEvent{ID: "evt_push_" + key, OrganizationID: "org_security", ProjectID: f.project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "run.checkpointed", SchemaVersion: 1, Actor: actor, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}})
				if err != nil {
					return err
				}
				event, err := content.PrepareEvent(t.Context(), "org_security", raw)
				if err != nil {
					return err
				}
				if key == "second" {
					event.PushKnown = false
					event.PushChangeID = ""
					event.PushVersionID = ""
					event.PushHeadSHA = ""
					event.PushExternalEffect = ""
					event.PushEffectState = ""
				}
				if _, err := w.RecordAttempt(t.Context(), leasestore.AttemptEvent{ID: run.AttemptID, RunID: run.RunID, Sequence: 1, Status: "running", Fence: leasestore.Fence{LeaseID: lease.LeaseID, FencingToken: lease.FencingToken}, PayloadObjectKey: event.PayloadObjectKey}); err != nil {
					return err
				}
				_, err = w.AppendEvent(t.Context(), event)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			recovered, err := native.RuntimeChange(t.Context(), issue.WorkItemID, change.ID)
			if err != nil {
				t.Fatal(err)
			}
			if recovered.Summary.Status != status || recovered.Summary.NativeReview != decision || recovered.Summary.Checks != conclusion {
				t.Fatalf("wrong semantic summary: %+v", recovered.Summary)
			}
			if len(recovered.PublicationPushes) != 1 || recovered.PublicationPushes[0].Change != reference || recovered.PublicationPushes[0].RecordingAttemptID != run.AttemptID || recovered.PublicationPushes[0].EffectID != run.Handoff.EffectID {
				t.Fatalf("wrong publication recovery: %+v", recovered.PublicationPushes)
			}
			if recovered.Change.ID != change.ID || recovered.Change.WorkItemID != issue.WorkItemID {
				t.Fatalf("wrong recovered identity: %+v", recovered.Change)
			}
			if len(recovered.Versions) != 2 || recovered.Versions[1].ID != latest.ID || recovered.Versions[1].Source == nil || *recovered.Versions[1].Source != *latest.Source || len(recovered.Reviews) != 1 || recovered.Reviews[0].Body != body || len(recovered.Checks) != 1 || len(recovered.Discussion) != discussionCount {
				t.Fatalf("incomplete source recovery: versions=%d reviews=%d checks=%d discussion=%d", len(recovered.Versions), len(recovered.Reviews), len(recovered.Checks), len(recovered.Discussion))
			}
			path := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes/" + change.ID + "?view=bounded"
			for _, selector := range []string{"&section=invalid", "&limit=bad", "&cursor=malformed", "&version_id=version_missing"} {
				response := f.request(t, owner, http.MethodGet, path+selector, nil)
				if response.Code < 400 || response.Code >= 500 {
					t.Fatalf("accepted invalid selector %s: %d", selector, response.Code)
				}
			}
			response = f.request(t, hostedSecurityUser{}, http.MethodGet, path, nil)
			if response.Code < 400 || response.Code >= 500 {
				t.Fatal("accepted unauthenticated bounded read")
			}

			response = f.request(t, owner, http.MethodGet, path+"&section=discussion&limit=1", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var page operatortool.ChangeResult
			decodeHubResponse(t, response, &page)
			if page.Detail.Summary.Status != status || page.Detail.Summary.NativeReview != decision || page.Detail.Summary.Checks != conclusion {
				t.Fatalf("paged read lost aggregate summary: %+v", page.Detail.Summary)
			}
			cursorRaw, err := base64.RawURLEncoding.DecodeString(page.Read.Sections["discussion"].NextCursor)
			if err != nil {
				t.Fatal(err)
			}
			var original boundedChangeRecordCursor
			if err := json.Unmarshal(cursorRaw, &original); err != nil {
				t.Fatal(err)
			}
			for _, test := range []struct {
				name   string
				mutate func(*boundedChangeRecordCursor)
			}{
				{"organization", func(c *boundedChangeRecordCursor) { c.Organization = "org_other" }},
				{"project", func(c *boundedChangeRecordCursor) { c.Project = "prj_other" }},
				{"item", func(c *boundedChangeRecordCursor) { c.Item = "wi_other" }},
				{"change", func(c *boundedChangeRecordCursor) { c.Change = "change_other" }},
				{"head", func(c *boundedChangeRecordCursor) { c.Head = "version_other" }},
				{"revision", func(c *boundedChangeRecordCursor) { c.Revision-- }},
				{"boundary", func(c *boundedChangeRecordCursor) { c.Through++ }},
				{"truncated_boundary", func(c *boundedChangeRecordCursor) { c.Through-- }},
				{"evidence_sequence", func(c *boundedChangeRecordCursor) { c.EvidenceSequence-- }},
			} {
				t.Run(test.name, func(t *testing.T) {
					cursor := original
					test.mutate(&cursor)
					encoded, err := encodeBoundedChangeRecordCursor(cursor)
					if err != nil {
						t.Fatal(err)
					}
					response := f.request(t, owner, http.MethodGet, path+"&section=discussion&cursor="+encoded, nil)
					if response.Code < 400 || response.Code >= 500 {
						t.Fatalf("accepted mismatched %s cursor", test.name)
					}
				})
			}
			response = f.request(t, owner, http.MethodGet, path+"&section=reviews&version_id=version_"+key+"_1", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var pinned operatortool.ChangeResult
			decodeHubResponse(t, response, &pinned)
			if pinned.Read.VersionID != "version_"+key+"_1" || len(pinned.Detail.Reviews) != 1 || pinned.Detail.Reviews[0].ID != "old_validator_"+key || len(pinned.Detail.Versions) != 1 || pinned.Detail.Versions[0].ID != pinned.Read.VersionID || pinned.Detail.Change.CurrentVersion != latest.ID {
				t.Fatal("historical version selection changed current head or leaked current reviews")
			}
			if firstChange != "" {
				cross := f.base + "/work-items/" + string(issue.WorkItemID) + "/changes/" + firstChange + "?view=bounded"
				response := f.request(t, owner, http.MethodGet, cross, nil)
				requireNativeStatus(t, response, http.StatusNotFound)
			} else {
				firstChange = change.ID
			}
			if key == "second" {
				response := f.request(t, owner, http.MethodGet, path+"&section=reviews&limit=1", nil)
				requireNativeStatus(t, response, http.StatusOK)
				var fragment operatortool.ChangeResult
				decodeHubResponse(t, response, &fragment)
				if fragment.Fragment == nil && fragment.Read.Sections["reviews"].NextCursor != "" {
					response = f.request(t, owner, http.MethodGet, path+"&section=reviews&limit=1&cursor="+fragment.Read.Sections["reviews"].NextCursor, nil)
					requireNativeStatus(t, response, http.StatusOK)
					decodeHubResponse(t, response, &fragment)
				}
				if fragment.Fragment == nil || fragment.Fragment.Offset != 0 || len(fragment.Fragment.Data) == 0 {
					t.Fatal("oversized review was not fragmented")
				}
				encoded := fragment.Read.Sections["reviews"].NextCursor
				raw, err := base64.RawURLEncoding.DecodeString(encoded)
				if err != nil {
					t.Fatal(err)
				}
				var cursor boundedChangeRecordCursor
				if err := json.Unmarshal(raw, &cursor); err != nil {
					t.Fatal(err)
				}
				cursor.Digest = strings.Repeat("0", 64)
				encoded, err = encodeBoundedChangeRecordCursor(cursor)
				if err != nil {
					t.Fatal(err)
				}
				response = f.request(t, owner, http.MethodGet, path+"&section=reviews&cursor="+encoded, nil)
				if response.Code < 400 || response.Code >= 500 {
					t.Fatal("accepted changed retained review digest")
				}
			}

			sourceResponse := f.request(t, owner, http.MethodGet, strings.Split(path, "?")[0]+"/versions/"+latest.ID+"/source", nil)
			requireNativeStatus(t, sourceResponse, http.StatusOK)
			if tracker.ChangeSourceDigest(sourceResponse.Body.Bytes()) != latest.Source.BundleSHA256 {
				t.Fatal("recovered source does not resolve to retained bundle")
			}
			legacyEvidence = true
			appendEvidence("review", tracker.ChangeReview{ID: "validator_" + key, VersionID: latest.ID, SessionMachineID: tracker.MachineID("machine_" + key), Decision: "changes_requested", Validator: &gate.ValidatorResult{Verdict: "fail", SessionID: 102}, Actor: actor, CreatedAt: now})
			for i := 0; i < 205; i++ {
				review := tracker.ChangeReview{ID: fmt.Sprintf("review_%s_%d", key, i), VersionID: latest.ID, Decision: "approved", Actor: actor, CreatedAt: now}
				if i == 0 {
					review.SessionMachineID = tracker.MachineID("machine_" + key)
					review.ImplementationReview = &gate.ValidatorResult{Verdict: "pass", SessionID: 103}
				}
				appendEvidence("review", review)
			}
			validatorFacts := func(machine tracker.MachineID) ([]eventstore.Evidence, bool) {
				t.Helper()
				var rows []eventstore.Evidence
				var complete bool
				if err := repository.ReadTenant(t.Context(), "org_security", corestore.Serving, func(r tenantstore.Reader) error {
					change, err := r.Change(t.Context(), change.ID)
					if err != nil {
						return err
					}
					rows, complete, err = r.(eventstore.ValidatorReviewReader).ValidatorReviewEvidence(t.Context(), change.ID, latest.ID, change.EvidenceSequence, machine, 102)
					return err
				}); err != nil {
					t.Fatal(err)
				}
				return rows, complete
			}
			if rows, complete := validatorFacts(tracker.MachineID("machine_" + key)); complete || len(rows) != 0 {
				t.Fatal("unknown review sessions were treated as complete")
			}
			appendEvidence("discussion", tracker.ChangeDiscussion{ID: "late_" + key, Body: "Later discussion", Actor: actor, CreatedAt: now})
			stale, err := encodeBoundedChangeRecordCursor(original)
			if err != nil {
				t.Fatal(err)
			}
			response = f.request(t, owner, http.MethodGet, path+"&section=discussion&cursor="+stale, nil)
			requireNativeStatus(t, response, http.StatusOK)
			var continued operatortool.ChangeResult
			decodeHubResponse(t, response, &continued)
			if continued.Detail.Summary.NativeReview != decision || continued.Detail.Summary.Checks != conclusion || continued.Detail.Summary.Status != status {
				t.Fatalf("append changed snapshot summary: %+v", continued.Detail.Summary)
			}
			snapshotDiscussion := len(page.Detail.Discussion) + len(continued.Detail.Discussion)
			for steps := 0; !continued.Read.Sections["discussion"].Complete; steps++ {
				if steps > discussionCount+5 {
					t.Fatal("snapshot traversal did not finish")
				}
				next := continued.Read.Sections["discussion"].NextCursor
				response = f.request(t, owner, http.MethodGet, path+"&section=discussion&cursor="+next, nil)
				requireNativeStatus(t, response, http.StatusOK)
				decodeHubResponse(t, response, &continued)
				snapshotDiscussion += len(continued.Detail.Discussion)
			}
			if snapshotDiscussion != discussionCount {
				t.Fatalf("snapshot discussion = %d, want %d", snapshotDiscussion, discussionCount)
			}
			response = f.request(t, owner, http.MethodGet, path+"&section=discussion&limit=1", nil)
			if response.Code < 400 {
				t.Fatal("unprojected history was treated as complete")
			}
			for i := 0; i < 205; i++ {
				raw, err := json.Marshal(tracker.CollaborationEvent{ID: fmt.Sprintf("evt_legacy_%s_%d", key, i), OrganizationID: "org_security", ProjectID: f.project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "run.checkpointed", SchemaVersion: 1, Actor: actor, RecordedAt: now})
				if err != nil {
					t.Fatal(err)
				}
				event, err := content.PrepareEvent(t.Context(), "org_security", raw)
				if err != nil {
					t.Fatal(err)
				}
				event.PushKnown = false
				if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvent(t.Context(), event); return err }); err != nil {
					t.Fatal(err)
				}
			}
			recoveryContent := boundedChangeSpool(t, f.details)
			backfiller := repository.(*postgres.Repository)
			total, err := backfiller.BackfillChangeRecoveryProjections(t.Context(), "org_security", recoveryContent, 7)
			if err != nil || total != 7 {
				t.Fatalf("first bounded batch = %d, %v", total, err)
			}
			batch, err := backfiller.PrepareChangeRecoveryProjections(t.Context(), "org_security", recoveryContent, 200)
			if err != nil {
				t.Fatal(err)
			}
			changed, err := admin.ExecContext(t.Context(), `UPDATE collaboration_events SET push_known=true WHERE organization_id='org_security' AND id=$1 AND NOT push_known`, "evt_legacy_"+key+"_204")
			if err != nil {
				t.Fatal(err)
			}
			n, err := changed.RowsAffected()
			if err != nil || n != 1 {
				t.Fatalf("conflict fixture update = %d, %v", n, err)
			}
			total++
			if n, err := backfiller.ApplyChangeRecoveryProjections(t.Context(), batch); n != 0 || !errors.Is(err, corestore.ErrRevisionConflict) {
				t.Fatalf("stale mixed batch = %d, %v", n, err)
			}
			var remaining int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM change_evidence WHERE organization_id='org_security' AND change_id=$1 AND kind='review' AND (review_kind='' OR NOT review_session_known)`, change.ID).Scan(&remaining); err != nil {
				t.Fatal(err)
			}
			if remaining != 199 {
				t.Fatalf("conflicting push committed review projections: remaining=%d", remaining)
			}
			for steps := 0; ; steps++ {
				if steps > 100 {
					t.Fatal("projection backfill did not converge")
				}
				count, err := backfiller.BackfillChangeRecoveryProjections(t.Context(), "org_security", recoveryContent, 7)
				if err != nil {
					t.Fatal(err)
				}
				if count < 0 || count > 7 {
					t.Fatalf("unbounded projection batch: %d", count)
				}
				total += count
				if count == 0 {
					break
				}
			}
			expected := 411
			if key == "second" {
				expected++
			}

			for _, statement := range []string{
				`UPDATE change_evidence SET actor_principal_id='changed' WHERE organization_id='org_security' AND change_id=$1 AND kind='review'`,
				`UPDATE change_evidence SET review_kind='validator' WHERE organization_id='org_security' AND change_id=$1 AND review_kind='human'`,
				`UPDATE change_evidence SET review_session_id=review_session_id+1 WHERE organization_id='org_security' AND change_id=$1 AND review_session_known`,
				`DELETE FROM change_evidence WHERE organization_id='org_security' AND change_id=$1`,
			} {
				if _, err := admin.ExecContext(t.Context(), statement, change.ID); err == nil {
					t.Fatal("projection migration weakened immutable evidence")
				}
			}
			legacyEvidence = false
			projected, err := native.RuntimeChange(t.Context(), issue.WorkItemID, change.ID)
			if err != nil || len(projected.PublicationPushes) != 1 || projected.PublicationPushes[0].Change != reference {
				t.Fatalf("historical push recovery = %+v, %v", projected.PublicationPushes, err)
			}
			if total != expected {
				t.Fatalf("projected %d records, want %d", total, expected)
			}
			facts, complete := validatorFacts(tracker.MachineID("machine_" + key))
			if !complete || len(facts) != 2 {
				t.Fatalf("selected validator facts = %d complete=%t", len(facts), complete)
			}
			for _, row := range facts {
				raw, err := recoveryContent.Evidence(t.Context(), row)
				if err != nil {
					t.Fatal(err)
				}
				var review tracker.ChangeReview
				if json.Unmarshal(raw, &review) != nil || review.SessionMachineID != tracker.MachineID("machine_"+key) || row.ReviewSessionMachine != review.SessionMachineID || row.ReviewSessionID != 102 && row.ReviewSessionID != 103 {
					t.Fatal("selected review lost verified session or machine identity")
				}
			}
			if rows, complete := validatorFacts("different-machine"); !complete || len(rows) != 1 || rows[0].ReviewKind != "implementation" {
				t.Fatal("validator selection mixed machine identities")
			}
			response = f.request(t, owner, http.MethodGet, path+"&section=discussion&limit=1", nil)
			requireNativeStatus(t, response, http.StatusOK)
			var aggregate operatortool.ChangeResult
			decodeHubResponse(t, response, &aggregate)
			if aggregate.Detail.Summary.NativeReview != "approved" || aggregate.Detail.Summary.Status != "needs_evidence" || aggregate.Detail.Summary.Checks != conclusion {
				t.Fatalf("latest human review hid validator failure: %+v", aggregate.Detail.Summary)
			}
			run.Sequence++
			if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
				raw, err := json.Marshal(tracker.CollaborationEvent{ID: "evt_unfenced_" + key, OrganizationID: "org_security", ProjectID: f.project, AggregateType: "issue", AggregateID: issue.WorkItemID, Type: "run.checkpointed", SchemaVersion: 1, Actor: actor, RecordedAt: now, Data: tracker.CollaborationData{Run: &run}})
				if err != nil {
					return err
				}
				event, err := content.PrepareEvent(t.Context(), "org_security", raw)
				if err != nil {
					return err
				}
				_, err = w.AppendEvent(t.Context(), event)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			unfenced, err := native.RuntimeChange(t.Context(), issue.WorkItemID, change.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(unfenced.PublicationPushes) != 0 {
				t.Fatalf("accepted checkpoint without attempt receipt: %+v", unfenced.PublicationPushes)
			}
			raw, err := json.Marshal(tracker.ChangeReview{ID: "review_corrupt_" + key, VersionID: latest.ID, Decision: "approved", Actor: actor, CreatedAt: now})
			if err != nil {
				t.Fatal(err)
			}
			bad, err := content.PrepareEvidence(t.Context(), eventstore.Evidence{OrganizationID: "org_security", ChangeID: change.ID, Kind: "review"}, raw)
			if err != nil {
				t.Fatal(err)
			}
			bad.ActorPrincipalID = "other-principal"
			if key == "second" {
				bad.ReviewKind = ""
			}
			if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error { _, err := w.AppendEvidence(t.Context(), bad); return err }); err != nil {
				t.Fatal(err)
			}
			if _, err := repository.(*postgres.Repository).BackfillChangeRecoveryProjections(t.Context(), "org_security", content, 7); key == "second" && !errors.Is(err, eventstore.ErrInvalidRecord) {
				t.Fatalf("corrupt projection backfill = %v", err)
			}
			_, err = native.RuntimeChange(t.Context(), issue.WorkItemID, change.ID)
			var refusal *hubapi.APIError
			if !errors.As(err, &refusal) || refusal.Status != http.StatusInternalServerError {
				t.Fatalf("mismatched typed evidence error = %v", err)
			}

		})
	}
}

type boundedChangeSpoolReader struct{ root string }

func (s boundedChangeSpoolReader) StoreDetail(context.Context, string, []byte) (attachment.ArchiveReference, error) {
	return attachment.ArchiveReference{}, errors.New("recovery spool is read only")
}
func (s boundedChangeSpoolReader) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if err := ref.Validate(organization); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(filepath.Join(s.root, ref.SHA256))
	if err != nil {
		return nil, err
	}
	verified, _, err := attachment.PrepareArchive(organization, 1, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	if verified.Key != ref.Key || verified.SHA256 != ref.SHA256 || verified.CompressedSize != ref.CompressedSize || verified.UncompressedSize != ref.UncompressedSize {
		return nil, artifact.ErrIntegrity
	}
	return raw, ctx.Err()
}
func boundedChangeSpool(t *testing.T, source *memoryRunDetails) *eventstore.Content {
	t.Helper()
	spool := boundedChangeSpoolReader{root: t.TempDir()}
	source.mu.Lock()
	defer source.mu.Unlock()
	for key, raw := range source.data {
		if err := os.WriteFile(filepath.Join(spool.root, filepath.Base(key)), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return eventstore.NewContent(spool)
}

func seedChangeReadCredential(t *testing.T, repository tenantstore.Repository, project tracker.ProjectID) tokenResponse {
	t.Helper()
	token, err := apikey.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	id := newNativeID("credential")
	hash := apikey.HashToken(token)
	now := time.Now().UTC()
	expires := now.Add(time.Hour)
	binding := runnerauth.NewBinding()
	enrollmentID := newNativeID("enrollment")
	operations := []string{runnerauth.Read}
	if err := repository.WriteTenant(t.Context(), "org_security", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_security", ID: id, Name: "Example read credential", Hash: hash, Fingerprint: tokenFingerprint(hash), Scope: "worker", KeyScope: "read", NativeOnly: true, CreatedAt: now, UpdatedAt: now, ExpiresAt: &expires, ProjectAccess: "selected", Kind: "service"}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_security", TokenID: id, ProjectID: project}); err != nil {
			return err
		}
		if err := w.RegisterMachine(t.Context(), leasestore.Machine{OrganizationID: "org_security", ID: string(binding.MachineID), Hostname: "example", Capacity: 1, Version: "example"}); err != nil {
			return err
		}
		if err := w.CreateRunnerEnrollment(t.Context(), runnerstore.Enrollment{OrganizationID: "org_security", ID: enrollmentID, RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenHash: apikey.HashToken(enrollmentID), Scope: "projects", ProjectIDs: []tracker.ProjectID{project}, Operations: operations, CreatedAt: now, ExpiresAt: expires, CreatedBy: "example-owner"}); err != nil {
			return err
		}
		if err := w.RedeemRunnerEnrollment(t.Context(), enrollmentID, binding.RunnerID, binding.MachineID, now); err != nil {
			return err
		}
		return w.CreateRunnerIdentity(t.Context(), runnerstore.Identity{OrganizationID: "org_security", RunnerID: binding.RunnerID, MachineID: binding.MachineID, TokenID: id, EnrollmentID: enrollmentID, Scope: "projects", State: "active", Operations: operations, CapacityLimit: 1, ReportedCapacity: 1, ProjectIDs: []tracker.ProjectID{project}, CreatedAt: now, ExpiresAt: expires, LastHeartbeatAt: now})
	}); err != nil {
		t.Fatal(err)
	}
	return tokenResponse{ID: id, Token: token}
}
