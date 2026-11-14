package workduplicates_test

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubscope"
	"github.com/digitaldrywood/detent/internal/hubserver/searchstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workduplicates"
	"github.com/digitaldrywood/detent/internal/issueorigin"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestNativeDuplicateCandidateCrowding(t *testing.T) {
	t.Parallel()
	for _, crowd := range []string{"project", "organization", "archived", "invisible", "partial coverage", "eligible"} {
		t.Run(crowd, func(t *testing.T) {
			t.Parallel()
			f := newDefaultNativeFixture(t)
			target := f.createIndependent(t, "Separate issue context")
			response := performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(target.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "crowding-comment"}, Body: "Repository association deadlocks"})
			requireNativeStatus(t, response, http.StatusOK)
			partial := f.createIndependent(t, "Repository association")
			credential, status, err := f.authenticateTenantAPIHash(t, f.project.OrganizationID, f.token)
			if err != nil || status != http.StatusOK {
				t.Fatalf("fixture credential=%d %v", status, err)
			}
			credential.HostedRole = "reporter"
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential, SourceActor: &target.Actor}
			creator := f
			if crowd == "project" || crowd == "organization" || crowd == "invisible" {
				organization := f.project.OrganizationID
				if crowd == "organization" {
					organization = f.second
				}
				creator = f.fixture(t, organization, "foreign-crowding")
				if crowd == "invisible" {
					foreign, _, err := f.authenticateTenantAPIHash(t, organization, creator.token)
					if err != nil {
						t.Fatal(err)
					}
					if err := f.repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
						return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: foreign.ID, ProjectID: f.project.ID})
					}); err != nil {
						t.Fatal(err)
					}
					creator.project, creator.base = f.project, f.base
				}
			}
			for number := 3; number <= 127; number++ {
				title := "Repository association deadlocks " + strconv.Itoa(number)
				if crowd == "partial coverage" {
					title = "association " + strconv.Itoa(number)
				}
				item := creator.createIndependent(t, title)
				if crowd == "archived" {
					response := performHubAPIRequest(t, creator.service, http.MethodPost, creator.base+"/work-items/"+string(item.WorkItemID)+"/archive", creator.token, archiveIssueRequest{Mutation: tracker.Mutation{IdempotencyKey: "archive-" + string(item.WorkItemID)}, ExpectedRevision: item.Revision})
					requireNativeStatus(t, response, http.StatusOK)
				}
			}
			for _, org := range []tracker.OrganizationID{f.project.OrganizationID, creator.project.OrganizationID} {
				refreshNativeDuplicateSearchFixture(t, f.hub, org)
			}
			for _, text := range []string{"repos associ deadlock", "repository association absent"} {
				ids := map[tracker.NativeWorkItemID]bool{}
				if err := f.repository.ReadTenant(t.Context(), scope.Organization, corestore.Serving, func(r tenantstore.Reader) error {
					hits, err := r.Search(t.Context(), searchstore.Query{Kind: "work", ProjectID: scope.Project, Reporter: new(target.Actor.PrincipalID), Text: text, Limit: 200})
					for _, hit := range hits {
						ids[hit.Document.WorkItemID] = true
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				want := 2
				if crowd == "eligible" || crowd == "partial coverage" {
					want = 127
				}
				if len(ids) != want || !ids[target.WorkItemID] || !ids[partial.WorkItemID] {
					t.Fatalf("search=%q ids=%v want %d matches", text, ids, want)
				}
			}
			if crowd == "eligible" {
				return
			}
			for _, request := range []tracker.CreateIssue{{Title: "Repository association deadlocks"}, {Title: "Separate observed symptom", Body: "Repository association deadlocks"}} {
				matches, err := workduplicates.Prepare(t.Context(), f.duplicateDeps(), scope, request, false)
				if err != nil || matches == nil || !matches.Available || len(matches.Matches) != 2 || !slices.ContainsFunc(matches.Matches, func(hit hubscope.PossibleDuplicate) bool { return hit.WorkItemID == target.WorkItemID }) || !slices.ContainsFunc(matches.Matches, func(hit hubscope.PossibleDuplicate) bool { return hit.WorkItemID == partial.WorkItemID }) {
					t.Fatalf("request=%+v matches=%+v err=%v", request, matches, err)
				}
			}
		})
	}
}

func TestNativeSearchScopedCost(t *testing.T) {
	for _, state := range []string{"ready", "rebuilding", "dirty"} {
		t.Run(state, func(t *testing.T) {
			f := newDefaultNativeFixture(t)
			target := f.createIndependent(t, "Repository association")
			foreign := f.fixture(t, f.project.OrganizationID, "foreign-search-cost")
			seed := foreign.createIndependent(t, "Repository association deadlocks")
			credential, status, err := f.authenticateTenantAPIHash(t, f.project.OrganizationID, f.token)
			if err != nil || status != http.StatusOK {
				t.Fatalf("fixture credential=%d %v", status, err)
			}
			scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
			admin := f.admin
			count := 1
			scores := map[string]float64{}
			for _, size := range []int{1, 300, 3000, 6000} {
				t.Run(strconv.Itoa(size), func(t *testing.T) {
					if size > count {
						if _, err := admin.ExecContext(t.Context(), `INSERT INTO issues(organization_id,project_id,native_id,number,revision,title,body_object_key,issue_contract_object_key,actor_kind,actor_principal_id,workflow_state_name,created_at,updated_at,native_created_at,native_updated_at,last_activity_at)
SELECT organization_id,project_id,'wi_scoped_cost_'||n,n,1,title,body_object_key,issue_contract_object_key,actor_kind,actor_principal_id,workflow_state_name,created_at,updated_at,native_created_at,native_updated_at,last_activity_at FROM issues CROSS JOIN generate_series($1::bigint,$2::bigint) n WHERE organization_id=$3 AND project_id=$4 AND native_id=$5`, count+1, size, f.project.OrganizationID, foreign.project.ID, seed.WorkItemID); err != nil {
							t.Fatal(err)
						}
						count = size
					}
					refreshNativeDuplicateSearchFixture(t, f.hub, f.project.OrganizationID)
					var indexed int
					if err := admin.QueryRowContext(t.Context(), "SELECT count(*) FROM search_documents WHERE organization_id=$1 AND project_id=$2 AND kind='issue' AND NOT placeholder AND indexed_revision=revision", f.project.OrganizationID, foreign.project.ID).Scan(&indexed); err != nil || indexed != size {
						t.Fatalf("foreign index population=%d want=%d error=%v", indexed, size, err)
					}
					if state != "ready" {
						response := performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(target.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "pending-cost-" + strconv.Itoa(size)}, ExpectedRevision: target.Revision, Labels: &[]string{"scope-marker-" + strconv.Itoa(size)}})
						requireNativeStatus(t, response, http.StatusOK)
						decodeHubResponse(t, response, &target)
						if state == "dirty" {
							response = performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(target.WorkItemID)+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "dirty-cost-comment-" + strconv.Itoa(size)}, Body: "Separate context"})
							requireNativeStatus(t, response, http.StatusOK)
						}
					}
					for _, text := range []string{"repos associ deadlock", "repository repository association association"} {
						if err := f.repository.ReadTenant(t.Context(), f.project.OrganizationID, corestore.Serving, func(r tenantstore.Reader) error {
							hits, err := r.Search(t.Context(), searchstore.Query{Kind: "work", ProjectID: f.project.ID, Text: text, Limit: 100})
							if err != nil {
								return err
							}
							ids := map[tracker.NativeWorkItemID]bool{}
							var score float64
							for _, hit := range hits {
								ids[hit.Document.WorkItemID] = true
								score += hit.Score
								if hit.Coverage != 2 {
									t.Fatalf("hits=%+v want 2 covered terms", hits)
								}
							}
							if len(ids) != 1 || !ids[target.WorkItemID] {
								t.Fatalf("foreign population %d displaced scoped result: %+v", size, hits)
							}
							if size == 1 {
								scores[text] = score
							}
							if state != "ready" && score != scores[text] {
								t.Fatalf("fallback score changed with foreign inventory: got=%v want=%v", score, scores[text])
							}
							reader := r.(searchstore.DuplicateSourceReader)
							availability, err := reader.DuplicateSourceState(t.Context(), f.project.ID)
							if err != nil {
								return err
							}
							if availability.Ready != (state == "ready") {
								t.Fatalf("index ready=%t for state %s", availability.Ready, state)
							}
							page, err := reader.DuplicateSources(t.Context(), searchstore.DuplicateSourceQuery{ProjectID: f.project.ID, Text: "Repository association deadlocks", Now: time.Now()})
							if state != "ready" {
								if !errors.Is(err, searchstore.ErrIndexPending) {
									t.Fatalf("incomplete duplicate source=%+v err=%v", page, err)
								}
								return nil
							}
							if err != nil {
								return err
							}
							if len(page.Sources) != 1 || page.Sources[0].Document.WorkItemID != target.WorkItemID {
								t.Fatalf("foreign population %d crowded duplicate source: %+v", size, page)
							}
							return nil
						}); err != nil {
							t.Fatal(err)
						}
					}
					duplicates, err := workduplicates.Prepare(t.Context(), f.duplicateDeps(), scope, tracker.CreateIssue{Title: "Repository association deadlocks"}, false)
					want := 1
					if state != "ready" {
						want = 0
					}
					if err != nil || duplicates == nil || len(duplicates.Matches) != want || duplicates.Available != (state == "ready") || want == 1 && duplicates.Matches[0].WorkItemID != target.WorkItemID {
						t.Fatalf("Hub duplicate outcome for state %s with %d indexed foreign issues: %+v error=%v", state, size, duplicates, err)
					}
				})
			}
		})
	}
}

func TestNativeMachineDuplicateCommentCandidate(t *testing.T) {
	f := newDefaultNativeFixture(t)
	stamp := func(body, fingerprint string) string {
		return issueorigin.Stamp(body+issueContractTestSections, issueorigin.Origin{Kind: "worker", Fingerprint: fingerprint})
	}
	read := func(t *testing.T, f nativeFixture, item tracker.NativeWorkItemID) tracker.NativeIssue {
		t.Helper()
		response := performHubAPIRequest(t, f.service, http.MethodGet, f.base+"/work-items/"+string(item), f.token, nil)
		requireNativeStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		decodeHubResponse(t, response, &issue)
		return issue
	}
	seed := func(t *testing.T, f nativeFixture, title, fingerprint, comment string) tracker.NativeIssue {
		t.Helper()
		issue := f.createIndependent(t, title)
		body := stamp("Initial diagnostic", fingerprint)
		path := f.base + "/work-items/" + string(issue.WorkItemID)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "body-" + title}, ExpectedRevision: issue.Revision, Body: &body}), http.StatusOK)
		if comment != "" {
			requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "comment-" + title}, Body: comment}), http.StatusOK)
		}
		refreshNativeDuplicateSearchFixture(t, f.hub, f.project.OrganizationID)
		return read(t, f, issue.WorkItemID)
	}
	prepare := func(t *testing.T, f nativeFixture, title, body string) *workduplicates.Search {
		t.Helper()
		credential, _, err := f.authenticateTenantAPIHash(t, f.project.OrganizationID, f.token)
		if err != nil {
			t.Fatal(err)
		}
		scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
		search, err := workduplicates.Prepare(t.Context(), f.duplicateDeps(), scope, tracker.CreateIssue{Title: title, Body: body}, true)
		if err != nil || !search.Available {
			t.Fatalf("search=%+v err=%v", search, err)
		}
		return search
	}
	requireMatches := func(t *testing.T, search *workduplicates.Search, want ...tracker.NativeWorkItemID) {
		t.Helper()
		var got []tracker.NativeWorkItemID
		for _, match := range search.Matches {
			if match.Why != "Matching machine fingerprint" {
				t.Fatalf("machine match without verified fingerprint: %+v", match)
			}
			got = append(got, match.WorkItemID)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("matches=%v want %v: %+v", got, want, search.Matches)
		}
	}
	diagnostic := "internal/example/pool.go:23:4: undefined: capacity"
	later := issueorigin.Stamp("internal/example/pool.go:31:2: undefined: capacity", issueorigin.Origin{Kind: "worker", Fingerprint: "later-failure"})
	owner := seed(t, f, "Original failure", "original-failure", diagnostic)
	t.Run("different title with the same defect", func(t *testing.T) {
		requireMatches(t, prepare(t, f, "Capacity symbol missing in pool", later), owner.WorkItemID)
	})
	t.Run("similar words with distinct fingerprints", func(t *testing.T) {
		requireMatches(t, prepare(t, f, "Original failure", issueorigin.Stamp("internal/example/pool.go:23:4: undefined: capacities", issueorigin.Origin{Kind: "worker", Fingerprint: "distinct-failure"})))
	})
	t.Run("other tenant stays separate", func(t *testing.T) {
		foreign := f.fixture(t, f.second, "Foreign duplicates")
		copied := seed(t, foreign, "Foreign original failure", "original-failure", diagnostic)
		requireMatches(t, prepare(t, f, "Later failure", later), owner.WorkItemID)
		requireMatches(t, prepare(t, foreign, "Later failure", later), copied.WorkItemID)
	})
	t.Run("closed item still matches", func(t *testing.T) {
		current := read(t, f, owner.WorkItemID)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, f.base+"/work-items/"+string(owner.WorkItemID)+"/workflow", f.token, tracker.Transition{Mutation: tracker.Mutation{IdempotencyKey: "close-owner"}, ExpectedRevision: current.Revision, State: "Done", Reason: "user_requested"}), http.StatusOK)
		refreshNativeDuplicateSearchFixture(t, f.hub, f.project.OrganizationID)
		search := prepare(t, f, "Later failure", later)
		requireMatches(t, search, owner.WorkItemID)
		if !search.Matches[0].Terminal {
			t.Fatalf("closed match=%+v", search.Matches[0])
		}
	})
	t.Run("document update changes the indexed fingerprint", func(t *testing.T) {
		updated := issueorigin.Stamp("Updated failure output", issueorigin.Origin{Kind: "worker", Fingerprint: "updated-failure"})
		item := seed(t, f, "Rewritten report", "rewritten-before", "")
		requireMatches(t, prepare(t, f, "Rewritten report", updated))
		body := stamp("Rewritten diagnostic", "updated-failure")
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, f.base+"/work-items/"+string(item.WorkItemID), f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "rewrite-body"}, ExpectedRevision: item.Revision, Body: &body}), http.StatusOK)
		refreshNativeDuplicateSearchFixture(t, f.hub, f.project.OrganizationID)
		requireMatches(t, prepare(t, f, "Unrelated title", updated), item.WorkItemID)
		requireMatches(t, prepare(t, f, "Rewritten report", issueorigin.Stamp("Old output", issueorigin.Origin{Kind: "worker", Fingerprint: "rewritten-before"})))
	})
}

func TestMachineDuplicateCommentOutsideParentAggregate(t *testing.T) {
	f := newDefaultNativeFixture(t)
	owner := f.createIndependent(t, "Long running failure")
	path := f.base + "/work-items/" + string(owner.WorkItemID)
	body := issueorigin.Stamp("Initial diagnostic"+issueContractTestSections, issueorigin.Origin{Kind: "worker", Fingerprint: "long-running-failure"})
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "long-owner-body"}, ExpectedRevision: owner.Revision, Body: &body}), http.StatusOK)
	for index := range 2 {
		filler := strings.Repeat("Unrelated progress notes for the long running failure. ", 700)
		requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "filler-" + strconv.Itoa(index)}, Body: filler}), http.StatusOK)
	}
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "late-diagnostic"}, Body: "internal/example/queue.go:40:7: undefined: drain"}), http.StatusOK)
	refreshNativeDuplicateSearchFixture(t, f.hub, f.project.OrganizationID)
	var issueHasDiagnostic bool
	if err := f.admin.QueryRowContext(t.Context(), "SELECT 'drain' = ANY(terms) FROM search_documents WHERE organization_id=$1 AND kind='issue' AND work_item_id=$2", f.project.OrganizationID, owner.WorkItemID).Scan(&issueHasDiagnostic); err != nil {
		t.Fatal(err)
	}
	if issueHasDiagnostic {
		t.Fatal("fixture diagnostic must lie outside the parent's 64 KiB aggregate")
	}
	credential, _, err := f.authenticateTenantAPIHash(t, f.project.OrganizationID, f.token)
	if err != nil {
		t.Fatal(err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	request := tracker.CreateIssue{Title: "Drain symbol missing", Body: issueorigin.Stamp("internal/example/queue.go:12:3: undefined: drain", issueorigin.Origin{Kind: "worker", Fingerprint: "later-drain-failure"})}
	matches := func(t *testing.T) []tracker.NativeWorkItemID {
		t.Helper()
		search, err := workduplicates.Prepare(t.Context(), f.duplicateDeps(), scope, request, true)
		if err != nil || !search.Available {
			t.Fatalf("search=%+v err=%v", search, err)
		}
		var got []tracker.NativeWorkItemID
		for _, match := range search.Matches {
			got = append(got, match.WorkItemID)
		}
		return got
	}
	if got := matches(t); !slices.Equal(got, []tracker.NativeWorkItemID{owner.WorkItemID}) {
		t.Fatalf("comment outside the aggregate matches=%v want %v", got, owner.WorkItemID)
	}
	fingerprints := searchstore.Prefix(f.project.OrganizationID, f.project.ID) + "f"
	if _, err := f.admin.ExecContext(t.Context(), "UPDATE search_documents SET source_vector=ts_delete(source_vector,ARRAY(SELECT v.lexeme FROM unnest(source_vector) v WHERE starts_with(v.lexeme,$3))),vector=ts_delete(vector,ARRAY(SELECT v.lexeme FROM unnest(vector) v WHERE starts_with(v.lexeme,$3))) WHERE organization_id=$1 AND kind='comment' AND work_item_id=$2", f.project.OrganizationID, owner.WorkItemID, fingerprints); err != nil {
		t.Fatal(err)
	}
	if got := matches(t); len(got) != 0 {
		t.Fatalf("legacy comment without fingerprint terms matched %v", got)
	}
	if _, err := f.admin.ExecContext(t.Context(), "UPDATE search_documents SET indexed_revision=0,refresh_order=nextval('search_refresh_order') WHERE kind='comment' AND NOT placeholder AND indexed_revision=revision AND terms && ARRAY['go','fail','goos','duplic','detent']::text[] AND organization_id=$1 AND work_item_id=$2", f.project.OrganizationID, owner.WorkItemID); err != nil {
		t.Fatal(err)
	}
	refreshNativeDuplicateSearchFixture(t, f.hub, f.project.OrganizationID)
	if got := matches(t); !slices.Equal(got, []tracker.NativeWorkItemID{owner.WorkItemID}) {
		t.Fatalf("bounded refresh of the legacy comment matches=%v want %v", got, owner.WorkItemID)
	}
}
