package workpage_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/hubserver/workitemread"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type listRequestKey struct{}

type countingDetails struct {
	attachment.DetailStore
	reads atomic.Int64
}

func (d *countingDetails) ReadDetail(ctx context.Context, organization string, ref attachment.ArchiveReference) ([]byte, error) {
	if ctx.Value(listRequestKey{}) != nil {
		d.reads.Add(1)
	}
	return d.DetailStore.ReadDetail(ctx, organization, ref)
}

func TestPlainWorkListReturnsSelectedBodiesWithinTheByteBudget(t *testing.T) {
	t.Parallel()
	details := &countingDetails{DetailStore: hubtest.NewDetails()}
	h := hubfixture.Open(t, hubfixture.Config{RunDetails: details})
	f := hubfixture.NewNative(t, h, "", "list-bodies")
	if err := h.Repository.WriteTenant(t.Context(), f.Project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
		record, err := w.ProjectRecord(t.Context(), f.Project.ID)
		if err != nil {
			return err
		}
		record.Settings.DuplicateChallenge = false
		_, err = w.UpdateProjectRecord(t.Context(), record)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	contract := []string{"linked_source", "external_references", "change"}
	bodies := map[tracker.NativeWorkItemID]string{}
	for index, size := range []int{2 << 10, 100 << 10, 2 << 10, 100 << 10, 256 << 10, 100 << 10, 2 << 10, 256 << 10, 2 << 10} {
		unit := "x"
		if size == 256<<10 {
			unit = "<\"\\"
		}
		body := strings.Repeat(unit, (size-len(hubfixture.IssueContractSections))/len(unit))
		body += strings.Repeat("x", size-len(hubfixture.IssueContractSections)-len(body)) + hubfixture.IssueContractSections
		response := f.Request(t, http.MethodPost, f.Base+"/work-items", tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: fmt.Sprintf("body-%d", index)}, Title: fmt.Sprintf("Body size %d", index), Body: body, State: "Todo"})
		hubfixture.RequireStatus(t, response, http.StatusOK)
		var issue tracker.NativeIssue
		hubfixture.Decode(t, response, &issue)
		bodies[issue.WorkItemID] = body
	}
	h.RefreshBoard(t, f.Project.OrganizationID)
	fits := func(issue tracker.NativeIssue) bool {
		issue.Body, issue.OmittedFields = bodies[issue.WorkItemID], contract
		raw, err := json.Marshal(issue)
		if err != nil {
			t.Fatal(err)
		}
		return workitemread.ListPageReserveBytes+len(raw) <= operatortool.MaxResultBytes
	}
	for _, limit := range []int{1, 3, 200} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			params := url.Values{"limit": {fmt.Sprint(limit)}}
			seen := map[tracker.NativeWorkItemID]bool{}
			pages := 0
			for {
				request := hubfixture.NewRequest(t, http.MethodGet, f.Base+"/work-items?"+params.Encode(), f.Token, nil)
				response := httptest.NewRecorder()
				before := details.reads.Load()
				h.Handler().ServeHTTP(response, request.WithContext(context.WithValue(request.Context(), listRequestKey{}, true)))
				reads := details.reads.Load() - before
				hubfixture.RequireStatus(t, response, http.StatusOK)
				if response.Body.Len() > operatortool.MaxResultBytes {
					t.Fatalf("page %d bytes=%d exceed the list budget", pages, response.Body.Len())
				}
				var page tracker.NativeIssuePage
				hubfixture.Decode(t, response, &page)
				if len(page.Items) == 0 || len(page.Items) > limit || reads > int64(len(page.Items))+1 {
					t.Fatalf("page %d items=%d payload reads=%d", pages, len(page.Items), reads)
				}
				complete := 0
				for _, issue := range page.Items {
					if issue.Body != "" {
						complete++
					}
				}
				if reads < int64(complete) {
					t.Fatalf("page %d completed %d bodies with %d payload reads", pages, complete, reads)
				}
				for _, issue := range page.Items {
					want, exists := bodies[issue.WorkItemID]
					if !exists || seen[issue.WorkItemID] {
						t.Fatalf("page %d duplicate or unexpected item %s", pages, issue.WorkItemID)
					}
					seen[issue.WorkItemID] = true
					if fits(issue) {
						if issue.Body != want || !slices.Equal(issue.OmittedFields, contract) {
							t.Fatalf("item %s lost its body: body_bytes=%d omitted=%v", issue.WorkItemID, len(issue.Body), issue.OmittedFields)
						}
						continue
					}
					if issue.Body != "" || !slices.Equal(issue.OmittedFields, append([]string{"body"}, contract...)) || len(page.Items) != 1 {
						t.Fatalf("oversized item %s: body_bytes=%d omitted=%v page_items=%d", issue.WorkItemID, len(issue.Body), issue.OmittedFields, len(page.Items))
					}
				}
				pages++
				if page.NextCursor == "" {
					break
				}
				if pages > len(bodies) {
					t.Fatal("cursor did not advance")
				}
				params.Set("cursor", page.NextCursor)
			}
			if len(seen) != len(bodies) || limit == 200 && pages < 3 {
				t.Fatalf("listed %d of %d items in %d pages", len(seen), len(bodies), pages)
			}
		})
	}
}
