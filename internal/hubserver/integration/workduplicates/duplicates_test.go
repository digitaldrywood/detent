package workduplicates_test

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
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
	owner := f.create(t, "Original failure")
	body := issueorigin.Stamp("Initial diagnostic"+issueContractTestSections, issueorigin.Origin{Kind: "worker", Fingerprint: "original-failure"})
	path := f.base + "/work-items/" + string(owner.WorkItemID)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPatch, path, f.token, tracker.UpdateIssue{Mutation: tracker.Mutation{IdempotencyKey: "duplicate-owner-body"}, ExpectedRevision: owner.Revision, Body: &body}), http.StatusOK)
	requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPost, path+"/comments", f.token, tracker.CreateComment{Mutation: tracker.Mutation{IdempotencyKey: "duplicate-owner-comment"}, Body: "internal/example/pool.go:23:4: undefined: capacity"}), http.StatusOK)
	refreshNativeDuplicateSearchFixture(t, f.hub, f.project.OrganizationID)
	credential, _, err := f.authenticateTenantAPIHash(t, f.project.OrganizationID, f.token)
	if err != nil {
		t.Fatal(err)
	}
	scope := hubscope.NativeScope{Organization: f.project.OrganizationID, Project: f.project.ID, Credential: credential}
	request := tracker.CreateIssue{Body: issueorigin.Stamp("internal/example/pool.go:31:2: undefined: capacity", issueorigin.Origin{Kind: "worker", Fingerprint: "later-failure"})}
	search, err := workduplicates.Prepare(t.Context(), f.duplicateDeps(), scope, request, true)
	if err != nil || !search.Available || len(search.Matches) != 1 || search.Matches[0].WorkItemID != owner.WorkItemID {
		t.Fatalf("search=%+v err=%v", search, err)
	}
}
