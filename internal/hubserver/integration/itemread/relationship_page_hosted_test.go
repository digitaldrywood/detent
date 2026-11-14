package itemread_test

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubmaintenance"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/hostedplans"
	"github.com/digitaldrywood/detent/internal/hubserver/hubrecords"
	"github.com/digitaldrywood/detent/internal/hubserver/hubtest"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/internal/hubdeps"
	"github.com/digitaldrywood/detent/internal/operatortool"
	"github.com/digitaldrywood/detent/internal/testenv"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type entryKeyCaller struct {
	hub    *hubfixture.Hosted
	signer ed25519.PrivateKey
}

type entryKeyHolder struct {
	user       hubfixture.HostedUser
	role       string
	permission apikey.Scope
	selection  apikey.ProjectContext
}

func (k entryKeyCaller) call(t *testing.T, holder entryKeyHolder, name string, arguments map[string]any) json.RawMessage {
	t.Helper()
	path := "/internal/v1/keys/call"
	body, err := json.Marshal(map[string]any{"name": name, "arguments": arguments, "connection_id": "relationship_page_" + holder.role})
	if err != nil {
		t.Fatal(err)
	}
	id, err := cloudassert.NewID()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	claims := cloudassert.Claims{Issuer: "entry", Audience: string(k.hub.Primary), ProviderOrganization: k.hub.Config.Hosted.WorkOSOrganizationID, Subject: holder.user.Identity.Subject, Role: holder.role, Generation: 1, Kind: cloudassert.KindKey, Method: http.MethodPost, Path: path, BodyDigest: cloudassert.BodyDigest(body), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id, Key: &apikey.KeyAuthority{ID: "key_" + holder.role, Kind: "personal", Permission: holder.permission, ProjectContext: holder.selection}}
	assertion, err := cloudassert.Sign(k.signer, claims)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)).WithContext(t.Context())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set(cloudassert.Header, assertion)
	response := httptest.NewRecorder()
	k.hub.Handler().ServeHTTP(response, request)
	hubfixture.RequireStatus(t, response, http.StatusOK)
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil || len(envelope.Data) == 0 {
		t.Fatalf("%s result %s: %v", name, response.Body.Bytes(), err)
	}
	return envelope.Data
}

func (k entryKeyCaller) file(t *testing.T, holder entryKeyHolder, project tracker.ProjectID, title string) tracker.NativeIssue {
	t.Helper()
	distinct := testenv.DistinctText(title, 24) + "\n"
	arguments := map[string]any{"project_id": string(project), "request_id": "file-" + title, "title": distinctTitle(title), "description": distinct + hubfixture.IssueBody(), "state": "Todo"}
	if holder.role == "reporter" {
		arguments["type"] = "bug"
		arguments["description"] = distinct + "## Steps to reproduce\nOpen the example page.\n## Expected\nThe page loads.\n## Actual\nThe page fails.\n## Where\nThe example settings page.\n## Environment\nExample browser."
		delete(arguments, "state")
	}
	var issue tracker.NativeIssue
	if err := json.Unmarshal(k.call(t, holder, operatortool.FileIssue, arguments), &issue); err != nil || issue.WorkItemID == "" {
		t.Fatalf("file %q: %v", title, err)
	}
	return issue
}

func (k entryKeyCaller) relate(t *testing.T, holder entryKeyHolder, project tracker.ProjectID, item *tracker.NativeIssue, related tracker.NativeWorkItemID, kind string) {
	t.Helper()
	arguments := map[string]any{"project_id": string(project), "request_id": "relate-" + string(item.WorkItemID) + "-" + string(related), "identifier": string(item.WorkItemID), "expected_revision": strconv.FormatInt(int64(item.Revision), 10), "related": string(related), "operation": "add", "relationship_type": kind}
	raw := k.call(t, holder, operatortool.SetDependency, arguments)
	var updated tracker.NativeIssue
	if err := json.Unmarshal(raw, &updated); err != nil || updated.Revision <= item.Revision {
		t.Fatalf("relate %s to %s: %s: %v", item.WorkItemID, related, raw, err)
	}
	item.Revision = updated.Revision
}

func (k entryKeyCaller) relationshipPage(t *testing.T, holder entryKeyHolder, project tracker.ProjectID, item tracker.NativeWorkItemID) relationshipPage {
	t.Helper()
	return decodeRelationshipPage(t, k.call(t, holder, operatortool.WorkRelationships, map[string]any{"project_id": string(project), "reference": string(item)}))
}

func TestHostedRelationshipPageLeavesNoTraceOfHiddenLinks(t *testing.T) {
	t.Parallel()
	signer := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x52}, ed25519.SeedSize))
	cfg := hubfixture.HostedSecurityConfig(hubfixture.NewHostedProvider())
	cfg.Hosted.SharedEntry = &hostedplans.HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{signer.Public().(ed25519.PublicKey)}, Generation: 1}
	h := hubfixture.OpenHosted(t, hubserver.WithDeps(cfg, hubdeps.Deps{RunMaintenance: func(ctx context.Context, _ *hubmaintenance.Scheduler, _ func(error)) error {
		<-ctx.Done()
		return ctx.Err()
	}}))
	hidden, visible := tracker.ProjectID("prj_b_hidden"), tracker.ProjectID("prj_c_visible")
	for _, project := range []struct {
		id   tracker.ProjectID
		name string
	}{{hidden, "Example hidden relationship project"}, {visible, "Example relationship project"}} {
		seeded := hubtest.Project(t, h.Repository, h.Primary, hubtest.ProjectOptions{ID: project.id, Name: project.name, States: hubrecords.NativeTriageStates(hubtest.States()), RequireDependencies: true})
		hubtest.Policy(t, h.Repository, h.Config.RunDetails, seeded, hubtest.PolicyOptions{Now: h.Now()})
	}
	everywhere := apikey.ProjectContext{Access: "all"}
	owner := entryKeyHolder{h.User(t, "owner", "owner", "owner@example.test", "", ""), "owner", apikey.ScopeWrite, everywhere}
	reporter := entryKeyHolder{h.User(t, "reporter", "reporter", "reporter@example.test", "", ""), "reporter", apikey.ScopeWrite, everywhere}
	member := entryKeyHolder{h.User(t, "member", "member", "member@example.test", "", ""), "member", apikey.ScopeRead, apikey.ProjectContext{Access: "selected", Projects: []string{string(visible)}}}
	for _, project := range []tracker.ProjectID{hidden, visible} {
		h.Grant(t, owner.user, project, true, false)
		h.Grant(t, reporter.user, project, true, false)
		h.Grant(t, member.user, project, false, false)
	}
	keys := entryKeyCaller{hub: h, signer: signer}
	subject := keys.file(t, reporter, visible, "Example reporter subject with a hidden boundary link")
	twin := keys.file(t, reporter, visible, "Example reporter subject without hidden links")
	for index := range operatortool.DefaultItemLimit - 1 {
		dependent := keys.file(t, reporter, visible, "Example reporter dependent number "+strconv.Itoa(index))
		for _, item := range []*tracker.NativeIssue{&subject, &twin} {
			keys.relate(t, owner, visible, &dependent, item.WorkItemID, "dependency")
		}
	}
	hiddenItem := keys.file(t, owner, hidden, "Example item another person filed in a second project")
	keys.relate(t, owner, visible, &subject, hiddenItem.WorkItemID, "related")
	for index := range 2 {
		related := keys.file(t, reporter, visible, "Example reporter related item number "+strconv.Itoa(index))
		for _, item := range []*tracker.NativeIssue{&subject, &twin} {
			keys.relate(t, owner, visible, item, related.WorkItemID, "related")
		}
	}
	cases := []struct {
		name   string
		holder entryKeyHolder
		hidden []string
	}{
		{"key restricted to the visible project", member, []string{string(hidden), string(hiddenItem.WorkItemID)}},
		{"reporter", reporter, []string{string(hiddenItem.WorkItemID)}},
	}
	for _, stage := range []string{"source", "stored page"} {
		if stage == "stored page" {
			h.RefreshPages(t, h.Primary)
		}
		for _, test := range cases {
			t.Run(stage+"/"+test.name, func(t *testing.T) {
				page := keys.relationshipPage(t, test.holder, visible, subject.WorkItemID)
				twinPage := keys.relationshipPage(t, test.holder, visible, twin.WorkItemID)
				if twinPage.cursor == nil {
					t.Fatalf("twin page has no continuation; the dataset does not reach the page boundary: %s", twinPage.body)
				}
				requireRelationshipPageMatches(t, page, twinPage, test.hidden)
			})
		}
	}
}
