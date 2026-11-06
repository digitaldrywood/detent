package hubserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"

	"github.com/digitaldrywood/detent/internal/apikey"
	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
	"github.com/digitaldrywood/detent/internal/hubserver/artifactstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/eventstore"
	"github.com/digitaldrywood/detent/internal/hubserver/projectstore"
	"github.com/digitaldrywood/detent/internal/hubserver/receiptstore"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestPostgresOrganizationVisibleRoutes(t *testing.T) {
	admin := postgresStartupTemplate.Database(t)
	now := time.Now().UTC().Truncate(time.Microsecond)
	cfg := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	cfg.Clock = func() time.Time { return now }
	key := hostedSharedKey(41)
	cfg.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	service, err := Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	repository := service.config.TenantRepository
	seedStartupOrganization(t, repository, "org_startup")
	seedStartupOrganization(t, repository, "org_second")
	for _, organization := range []tracker.OrganizationID{"org_startup", "org_second"} {
		if err := repository.WriteTenant(t.Context(), organization, corestore.Serving, func(w tenantstore.Writer) error {
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "credential_owner", Name: "Example owner", Hash: "example", Scope: "admin", KeyScope: "admin", ProjectAccess: "all", Kind: "personal", UserID: "user_owner", MembershipID: "membership_owner", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_owner", Email: "owner@example.test", MembershipID: "membership_owner", Role: "owner", Active: true, PrincipalID: "credential_owner", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "credential_reporter", Name: "Example reporter", Hash: "example_reporter", Scope: "operator", KeyScope: "write", ProjectAccess: "selected", Kind: "personal", UserID: "user_reporter", MembershipID: "membership_reporter", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: organization, UserID: "user_reporter", Email: "reporter@example.test", MembershipID: "membership_reporter", Role: "member", Active: true, PrincipalID: "credential_reporter", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: organization, ID: "credential_publisher", Name: "Example publisher", Hash: apikey.HashToken("publisher_" + string(organization)), Scope: "worker", NativeOnly: true, ProjectAccess: "selected", Kind: "service", CreatedAt: now, UpdatedAt: now}); err != nil {
				return err
			}
			for i, id := range []tracker.ProjectID{"project_first", "project_hidden", "project_last"} {
				if _, err := w.CreateProjectRecord(t.Context(), projectstore.Record{Core: corestore.Project{OrganizationID: organization, ID: id, Name: string(id), Profile: "native", SchedulingRank: int64(i)}, States: nativeFixtureStates()}); err != nil {
					return err
				}
				if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "user_owner", ProjectID: id, CanWrite: true}); err != nil {
					return err
				}
				if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "credential_owner", ProjectID: id}); err != nil {
					return err
				}
			}
			if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: organization, UserID: "user_reporter", ProjectID: "project_first", CanWrite: true}); err != nil {
				return err
			}
			if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "credential_reporter", ProjectID: "project_first"}); err != nil {
				return err
			}
			return w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: organization, TokenID: "credential_publisher", ProjectID: "project_first"})
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		if err := w.CreateAccessCredential(t.Context(), accessstore.Credential{OrganizationID: "org_startup", ID: "credential_member", Name: "Example member", Hash: "example_member", Scope: "operator", KeyScope: "write", ProjectAccess: "selected", Kind: "personal", UserID: "user_member", MembershipID: "membership_member", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessMember(t.Context(), accessstore.Member{OrganizationID: "org_startup", UserID: "user_member", Email: "member@example.test", MembershipID: "membership_member", Role: "member", Active: true, PrincipalID: "credential_member", CreatedAt: now, UpdatedAt: now}); err != nil {
			return err
		}
		if err := w.CreateAccessProjectGrant(t.Context(), accessstore.ProjectGrant{OrganizationID: "org_startup", UserID: "user_member", ProjectID: "project_first", CanWrite: true}); err != nil {
			return err
		}
		if err := w.CreateAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: "org_startup", TokenID: "credential_member", ProjectID: "project_first"}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	secondConfig := startupConfig(startupConnection(t, admin, "detent_runtime_v2", ""))
	secondConfig.Clock = func() time.Time { return now }
	secondConfig.Hosted.OrganizationID = "org_second"
	secondConfig.Hosted.SharedEntry = &HostedSharedEntry{Issuer: "entry", PublicKeys: []ed25519.PublicKey{key.Public().(ed25519.PublicKey)}, Generation: 1}
	secondService, err := Open(t.Context(), secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := secondService.Close(); err != nil {
			t.Error(err)
		}
	})
	organization := "org_startup"
	subject, role := "user_owner", "owner"
	requestContext := t.Context()
	request := func(method, path string, body any) *httptest.ResponseRecorder {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, err = json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
		}
		id, err := cloudassert.NewID()
		if err != nil {
			t.Fatal(err)
		}
		claims := cloudassert.Claims{Issuer: "entry", Audience: organization, Generation: 1, Kind: cloudassert.KindBrowser, Method: method, Path: path, BodyDigest: cloudassert.BodyDigest(raw), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: id, Subject: subject, Email: subject + "@example.test", ProviderOrganization: "provider_" + organization, ProviderSession: "session_" + subject, SessionCreatedAt: now, SessionExpiresAt: now.Add(time.Hour), AccessExpiresAt: now.Add(time.Hour), Role: role, Binding: cloudassert.AuthorizationBinding("shared-"+subject, organization, "session_"+subject), CSRF: cloudassert.CSRFToken("shared-"+subject, organization)}
		assertion, err := cloudassert.Sign(key, claims)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(method, path, bytes.NewReader(raw)).WithContext(requestContext)
		req.Header.Set(cloudassert.Header, assertion)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", claims.CSRF)
		response := httptest.NewRecorder()
		if organization == "org_second" {
			secondService.Handler().ServeHTTP(response, req)
		} else {
			service.Handler().ServeHTTP(response, req)
		}
		return response
	}
	var firstArtifactToken string
	for _, org := range []string{"org_startup", "org_second"} {
		organization = org
		selected := service
		if org == "org_second" {
			selected = secondService
		}
		provider := selected.config.Hosted.Provider.(*hostedSecurityProvider)
		membership := auth.Membership{ID: "membership_owner", UserID: "user_owner", OrganizationID: "provider_" + org, Status: "active"}
		membership.Role.Slug = "owner"
		provider.mu.Lock()
		provider.members[membership.ID] = membership
		provider.sessions["session_user_owner"] = auth.HostedIdentity{Subject: "user_owner", OrganizationID: "provider_" + org, SessionID: "session_user_owner", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		provider.mu.Unlock()
		base := "/organizations/" + org + "/api/v2/organizations/" + org + "/projects/project_first/artifact-services"
		requireNativeStatus(t, request(http.MethodGet, base, nil), http.StatusOK)
		body := map[string]any{"service_id": "service_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "origin": "https://" + org + ".example.test", "mode": "customer", "publisher_token_id": "credential_publisher", "idempotency_key": "bind-example"}
		bound := request(http.MethodPut, strings.Replace(base, "/artifact-services", "/onboarding/artifact-services", 1)+"/service_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", body)
		requireNativeStatus(t, bound, http.StatusOK)
		replay := request(http.MethodPut, strings.Replace(base, "/artifact-services", "/onboarding/artifact-services", 1)+"/service_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", body)
		requireNativeStatus(t, replay, http.StatusOK)
		if !bytes.Equal(bound.Body.Bytes(), replay.Body.Bytes()) {
			t.Fatal("binding replay changed")
		}
		listed := request(http.MethodGet, base, nil)
		requireNativeStatus(t, listed, http.StatusOK)
		var bindings []struct {
			Origin    string `json:"origin"`
			Publisher string `json:"publisher_token_id"`
		}
		if err := json.Unmarshal(listed.Body.Bytes(), &bindings); err != nil || len(bindings) != 1 || bindings[0].Origin != body["origin"] || bindings[0].Publisher != "" {
			t.Fatalf("binding list: %s error=%v", listed.Body.String(), err)
		}
		if err := repository.ReadTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(r tenantstore.Reader) error {
			binding, err := r.ArtifactService(t.Context(), "project_first", body["service_id"].(string))
			if err != nil {
				return err
			}
			if binding.Origin != body["origin"] || binding.PublisherTokenID != "credential_publisher" {
				t.Fatalf("stored binding: %+v", binding)
			}
			_, err = r.ArtifactService(t.Context(), "project_hidden", binding.ServiceID)
			if !errors.Is(err, corestore.ErrNotFound) {
				t.Fatalf("cross-project binding: %v", err)
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		publisherRequest := func(serviceID, raw string) *httptest.ResponseRecorder {
			req := httptest.NewRequest(http.MethodPost, base+"/"+serviceID+"/authorize", strings.NewReader(raw))
			assertionID, err := cloudassert.NewID()
			if err != nil {
				t.Fatal(err)
			}
			issued := time.Now().UTC()
			assertion, err := cloudassert.Sign(key, cloudassert.Claims{Issuer: "entry", Audience: org, Generation: 1, Kind: cloudassert.KindMachine, Method: http.MethodPost, Path: req.URL.Path, BodyDigest: cloudassert.BodyDigest([]byte(raw)), IssuedAt: issued, ExpiresAt: issued.Add(20 * time.Second), ID: assertionID})
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set(cloudassert.Header, assertion)
			req.Header.Set("Authorization", "Bearer publisher_"+org)
			req.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			handler := service.Handler()
			if org == "org_second" {
				handler = secondService.Handler()
			}
			handler.ServeHTTP(response, req)
			return response
		}
		requireNativeStatus(t, publisherRequest(body["service_id"].(string), "["), http.StatusUnprocessableEntity)
		requireNativeStatus(t, publisherRequest("service_missing", "["), http.StatusNotFound)

		item := tracker.NativeWorkItemID("wi_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		version, err := eventstore.NewContent(cfg.RunDetails).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: tracker.OrganizationID(org), ProjectID: "project_first", WorkItemID: item, RecordID: string(item), Revision: 1}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: tracker.OrganizationID(org), ProjectID: "project_first", NativeID: item, Number: 1, Title: "Example artifact", WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("credential_owner")}); err != nil {
				return err
			}
			_, err := w.CreateCollaborationVersion(t.Context(), version)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		artifactID := "artifact_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_references(organization_id,project_id,work_item_id,service_id,artifact_id,revision,manifest_id,schema_version,sha256,kind,state,availability,bytes,objects,expires_at,observed_at) VALUES($1,'project_first',$2,$3,$4,1,$5,1,$6,'log','complete','available',12,1,$7,$8)`, org, item, body["service_id"], artifactID, artifact.NewID("manifest"), artifact.Digest([]byte(org)), now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		accessPath := strings.TrimSuffix(base, "/artifact-services") + "/work-items/" + string(item) + "/artifacts/" + artifactID + "/access"
		granted := request(http.MethodPost, accessPath, map[string]int{"revision": 1})
		requireNativeStatus(t, granted, http.StatusOK)
		var grant artifact.Grant
		if err := json.Unmarshal(granted.Body.Bytes(), &grant); err != nil {
			t.Fatal(err)
		}
		if grant.Token == "" || grant.Origin != body["origin"] || grant.SHA256 != artifact.Digest([]byte(org)) || !grant.ExpiresAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("unexpected grant: %+v", grant)
		}
		authorize, err := json.Marshal(artifact.ReadAuthorization{Token: grant.Token, ArtifactID: artifactID, Revision: 1})
		if err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, publisherRequest(body["service_id"].(string), string(authorize)), http.StatusNoContent)
		provider.mu.Lock()
		savedSession := provider.sessions["session_user_owner"]
		delete(provider.sessions, "session_user_owner")
		provider.mu.Unlock()
		requireNativeStatus(t, publisherRequest(body["service_id"].(string), string(authorize)), http.StatusNotFound)
		provider.mu.Lock()
		provider.sessions["session_user_owner"] = savedSession
		provider.mu.Unlock()
		if firstArtifactToken != "" {
			foreign, err := json.Marshal(artifact.ReadAuthorization{Token: firstArtifactToken, ArtifactID: artifactID, Revision: 1})
			if err != nil {
				t.Fatal(err)
			}
			requireNativeStatus(t, publisherRequest(body["service_id"].(string), string(foreign)), http.StatusNotFound)
		}
		firstArtifactToken = grant.Token

		reporterItem := tracker.NativeWorkItemID("wi_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		reporterArtifact := "artifact_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		reporterVersion, err := eventstore.NewContent(cfg.RunDetails).PrepareCollaborationVersion(t.Context(), eventstore.CollaborationVersion{OrganizationID: tracker.OrganizationID(org), ProjectID: "project_first", WorkItemID: reporterItem, RecordID: string(reporterItem), Revision: 1}, []byte(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			if _, err := w.CreateWorkItem(t.Context(), corestore.WorkItem{OrganizationID: tracker.OrganizationID(org), ProjectID: "project_first", NativeID: reporterItem, Number: 2, Title: "Reporter artifact", WorkflowStateName: new("Todo"), ActorKind: new("human"), ActorPrincipalID: new("credential_reporter")}); err != nil {
				return err
			}
			_, err := w.CreateCollaborationVersion(t.Context(), reporterVersion)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_references(organization_id,project_id,work_item_id,service_id,artifact_id,revision,manifest_id,schema_version,sha256,kind,state,availability,bytes,objects,expires_at,observed_at) VALUES($1,'project_first',$2,$3,$4,1,$5,1,$6,'log','complete','available',12,1,$7,$8)`, org, reporterItem, body["service_id"], reporterArtifact, artifact.NewID("manifest"), artifact.Digest([]byte(org+"reporter")), now.Add(time.Hour), now); err != nil {
			t.Fatal(err)
		}
		reporterMembership := auth.Membership{ID: "membership_reporter", UserID: "user_reporter", OrganizationID: "provider_" + org, Status: "active"}
		reporterMembership.Role.Slug = "member"
		provider.mu.Lock()
		provider.members[reporterMembership.ID] = reporterMembership
		provider.sessions["session_user_reporter"] = auth.HostedIdentity{Subject: "user_reporter", OrganizationID: "provider_" + org, SessionID: "session_user_reporter", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}
		provider.mu.Unlock()
		redeem := func(token, id string) *httptest.ResponseRecorder {
			t.Helper()
			raw, err := json.Marshal(artifact.ReadAuthorization{Token: token, ArtifactID: id, Revision: 1})
			if err != nil {
				t.Fatal(err)
			}
			return publisherRequest(body["service_id"].(string), string(raw))
		}
		subject, role = "user_reporter", "member"
		memberGranted := request(http.MethodPost, accessPath, map[string]int{"revision": 1})
		requireNativeStatus(t, memberGranted, http.StatusOK)
		var memberGrant artifact.Grant
		if err := json.Unmarshal(memberGranted.Body.Bytes(), &memberGrant); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, redeem(memberGrant.Token, artifactID), http.StatusNoContent)
		reporterMembership.Role.Slug = "reporter"
		provider.mu.Lock()
		provider.members[reporterMembership.ID] = reporterMembership
		provider.mu.Unlock()
		role = "reporter"
		requireNativeStatus(t, redeem(memberGrant.Token, artifactID), http.StatusNotFound)
		requireNativeStatus(t, request(http.MethodPost, accessPath, map[string]int{"revision": 1}), http.StatusNotFound)
		reporterGranted := request(http.MethodPost, strings.Replace(accessPath, string(item)+"/artifacts/"+artifactID, string(reporterItem)+"/artifacts/"+reporterArtifact, 1), map[string]int{"revision": 1})
		requireNativeStatus(t, reporterGranted, http.StatusOK)
		var reporterGrant artifact.Grant
		if err := json.Unmarshal(reporterGranted.Body.Bytes(), &reporterGrant); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, redeem(reporterGrant.Token, reporterArtifact), http.StatusNoContent)
		subject, role = "user_owner", "owner"
		if _, err := admin.ExecContext(t.Context(), `DELETE FROM artifact_grants WHERE organization_id=$1 AND principal_id='credential_reporter'`, org); err != nil {
			t.Fatal(err)
		}

		floodService, quietService := body["service_id"].(string), "service_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		quietArtifact := "artifact_ffffffffffffffffffffffffffffffff"
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_services(organization_id,project_id,id,origin,binding_mode,hosted_opt_in,publisher_token_id) VALUES($1,'project_first',$2,'https://quiet.example.test','customer',false,'credential_publisher')`, org, quietService); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_references(organization_id,project_id,work_item_id,service_id,artifact_id,revision,manifest_id,schema_version,sha256,kind,state,availability,bytes,objects,expires_at,observed_at) SELECT $1,'project_first',$2,$3,'artifact_'||lpad(n::text,32,'0'),1,'manifest_'||$1||'_flood_'||n,1,$4,'log','complete','available',12,1,$5::timestamptz,$6::timestamptz FROM generate_series(1,64) n UNION ALL SELECT $1,'project_first',$2,$7,$8,1,'manifest_'||$1||'_quiet',1,$4,'log','complete','available',12,1,$5::timestamptz,$6::timestamptz`, org, item, floodService, artifact.Digest([]byte(org)), now.Add(time.Hour), now, quietService, quietArtifact); err != nil {
			t.Fatal(err)
		}
		floodList := request(http.MethodGet, strings.TrimSuffix(accessPath, "/"+artifactID+"/access"), nil)
		requireNativeStatus(t, floodList, http.StatusOK)
		var refs []artifact.Reference
		if err := json.Unmarshal(floodList.Body.Bytes(), &refs); err != nil {
			t.Fatal(err)
		}
		quietListed := slices.ContainsFunc(refs, func(ref artifact.Reference) bool {
			return ref.ServiceID == quietService && ref.ArtifactID == quietArtifact
		})
		if len(refs) != 64 || !quietListed || !slices.IsSortedFunc(refs, func(a, b artifact.Reference) int { return strings.Compare(a.ArtifactID, b.ArtifactID) }) {
			t.Fatalf("%s flooded artifact list returned %d references, quiet service listed=%t", org, len(refs), quietListed)
		}

		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			return w.DeleteAccessTokenGrant(t.Context(), "credential_owner", "project_first")
		}); err != nil {
			t.Fatal(err)
		}
		requireNativeStatus(t, publisherRequest(body["service_id"].(string), string(authorize)), http.StatusNotFound)
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Serving, func(w tenantstore.Writer) error {
			return w.SetAccessTokenGrant(t.Context(), accessstore.TokenGrant{OrganizationID: tracker.OrganizationID(org), TokenID: "credential_owner", ProjectID: "project_first"})
		}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		requireNativeStatus(t, publisherRequest(body["service_id"].(string), string(authorize)), http.StatusNotFound)
		now = now.Add(-time.Minute)
		body["origin"] = "https://changed.example.test"
		requireNativeStatus(t, request(http.MethodPut, strings.Replace(base, "/artifact-services", "/onboarding/artifact-services", 1)+"/service_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", body), http.StatusConflict)
		body["idempotency_key"] = "missing-publisher"
		body["publisher_token_id"] = "credential_missing"
		requireNativeStatus(t, request(http.MethodPut, strings.Replace(base, "/artifact-services", "/onboarding/artifact-services", 1)+"/service_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", body), http.StatusNotFound)

		grantRows := func(principal string) int {
			t.Helper()
			var count int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM artifact_grants WHERE organization_id=$1 AND principal_id=$2`, org, principal).Scan(&count); err != nil {
				t.Fatal(err)
			}
			return count
		}
		expireGrants := func() {
			t.Helper()
			if _, err := admin.ExecContext(t.Context(), `UPDATE artifact_grants SET expires_at=$2 WHERE organization_id=$1`, org, now); err != nil {
				t.Fatal(err)
			}
		}
		expireGrants()
		if _, err := admin.ExecContext(t.Context(), `INSERT INTO artifact_grants(organization_id,project_id,token_hash,principal_id,service_id,artifact_id,revision,expires_at,hosted_session_hash) SELECT $1,'project_first',encode(sha256(convert_to($1||':backlog:'||n,'UTF8')),'hex'),'credential_owner',$2,$3,1,$4,'' FROM generate_series(1,300) n`, org, body["service_id"], artifactID, now.Add(-time.Hour)); err != nil {
			t.Fatal(err)
		}
		backlog := grantRows("credential_owner")
		requireNativeStatus(t, request(http.MethodPost, accessPath, map[string]int{"revision": 1}), http.StatusOK)
		if got, want := grantRows("credential_owner"), backlog-artifactstore.ReadGrantPruneBatch+1; got != want {
			t.Fatalf("%s owner grant rows after bounded request cleanup=%d, want %d", org, got, want)
		}
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(org), corestore.Background, func(w tenantstore.Writer) error {
			pruned, err := w.PruneExpiredArtifactReadGrants(t.Context(), now, artifactstore.MaxReadGrantPruneBatch)
			if want := int64(backlog - artifactstore.ReadGrantPruneBatch); pruned != want {
				t.Errorf("%s background pruned=%d, want %d", org, pruned, want)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if got := grantRows("credential_owner"); got != 1 {
			t.Fatalf("%s owner grant rows after background prune=%d, want 1", org, got)
		}
		for range artifactstore.ReadGrantLimit - 1 {
			requireNativeStatus(t, request(http.MethodPost, accessPath, map[string]int{"revision": 1}), http.StatusOK)
		}
		requireNativeStatus(t, request(http.MethodPost, accessPath, map[string]int{"revision": 1}), http.StatusTooManyRequests)
		if got := grantRows("credential_owner"); got != artifactstore.ReadGrantLimit {
			t.Fatalf("%s owner grant rows=%d, want %d", org, got, artifactstore.ReadGrantLimit)
		}
		if org == "org_startup" {
			subject, role = "user_member", "member"
			requireNativeStatus(t, request(http.MethodPost, accessPath, map[string]int{"revision": 1}), http.StatusOK)
			subject, role = "user_owner", "owner"
			if got := grantRows("credential_member"); got != 1 {
				t.Fatalf("member grant rows=%d, want 1", got)
			}
		}
		expireGrants()
		requireNativeStatus(t, request(http.MethodPost, accessPath, map[string]int{"revision": 1}), http.StatusOK)
		if got := grantRows("credential_owner"); got != 1 {
			t.Fatalf("%s owner grant rows after expiry=%d, want 1", org, got)
		}
	}
	organization = "org_startup"
	base := "/organizations/org_startup/api/v2/organizations/org_startup"
	response := request(http.MethodGet, base+"/name", nil)
	requireNativeStatus(t, response, http.StatusOK)
	body := map[string]any{"idempotency_key": "rename", "name": "Renamed example"}
	first := request(http.MethodPut, base+"/name", body)
	requireNativeStatus(t, first, http.StatusOK)
	replay := request(http.MethodPut, base+"/name", body)
	requireNativeStatus(t, replay, http.StatusOK)
	if !bytes.Equal(first.Body.Bytes(), replay.Body.Bytes()) {
		t.Fatal("rename replay changed")
	}
	response = request(http.MethodGet, base+"/name", nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Contains(response.Body.Bytes(), []byte("Renamed example")) {
		t.Fatal("rename was not persisted")
	}
	if err := repository.ReadTenant(t.Context(), "org_second", corestore.Serving, func(r tenantstore.Reader) error {
		row, err := r.AccessOrganization(t.Context())
		if row.Name != "Example" {
			t.Error("rename crossed organization boundary")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	secondBase := "/organizations/org_second/api/v2/organizations/org_second"
	organization = "org_second"
	requireNativeStatus(t, request(http.MethodPut, secondBase+"/name", map[string]any{"idempotency_key": "rename", "name": "Second example"}), http.StatusOK)
	requireNativeStatus(t, request(http.MethodPut, secondBase+"/project-rank", map[string]any{"expected_revision": 1, "project_ids": []tracker.ProjectID{"project_last", "project_first", "project_hidden"}}), http.StatusOK)
	secondRank := request(http.MethodGet, secondBase+"/project-rank", nil)
	requireNativeStatus(t, secondRank, http.StatusOK)
	var other organizationProjectRank
	decodeHubResponse(t, secondRank, &other)
	if other.Revision != 2 || !slices.Equal(other.ProjectIDs, []tracker.ProjectID{"project_last", "project_first", "project_hidden"}) {
		t.Fatalf("second organization rank=%+v", other)
	}
	requireNativeStatus(t, request(http.MethodGet, base+"/name", nil), http.StatusNotFound)
	organization = "org_startup"
	response = request(http.MethodGet, base+"/name", nil)
	requireNativeStatus(t, response, http.StatusOK)
	if !bytes.Contains(response.Body.Bytes(), []byte("Renamed example")) {
		t.Fatal("second rename crossed organization boundary")
	}
	response = request(http.MethodGet, base+"/project-rank", nil)
	requireNativeStatus(t, response, http.StatusOK)
	var rank organizationProjectRank
	decodeHubResponse(t, response, &rank)
	if len(rank.ProjectIDs) != 3 {
		t.Fatalf("rank projects=%v", rank.ProjectIDs)
	}
	reverse := []tracker.ProjectID{"project_last", "project_hidden", "project_first"}
	response = request(http.MethodPut, base+"/project-rank", map[string]any{"expected_revision": rank.Revision, "project_ids": reverse})
	requireNativeStatus(t, response, http.StatusOK)
	response = request(http.MethodGet, base+"/project-rank", nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &rank)
	if rank.ProjectIDs[0] != "project_last" || rank.ProjectIDs[2] != "project_first" {
		t.Fatalf("rank not persisted: %+v", rank)
	}
	requireNativeStatus(t, request(http.MethodPut, base+"/project-rank", map[string]any{"expected_revision": rank.Revision - 1, "project_ids": reverse}), http.StatusConflict)
	subject, role = "user_member", "member"
	for _, path := range []string{"/name", "/project-rank"} {
		requireNativeStatus(t, request(http.MethodGet, base+path, nil), http.StatusOK)
	}
	response = request(http.MethodGet, base+"/project-rank", nil)
	requireNativeStatus(t, response, http.StatusOK)
	decodeHubResponse(t, response, &rank)
	if len(rank.ProjectIDs) != 1 || rank.ProjectIDs[0] != "project_first" {
		t.Fatalf("hidden projects leaked: %+v", rank)
	}
	requireNativeStatus(t, request(http.MethodPut, base+"/name", body), http.StatusForbidden)
	requireNativeStatus(t, request(http.MethodPut, base+"/project-rank", map[string]any{"expected_revision": rank.Revision, "project_ids": rank.ProjectIDs}), http.StatusForbidden)
	subject, role = "user_owner", "owner"

	ownerScope := nativeScope{organization: "org_startup", credential: apiCredential{ID: "credential_owner", Hash: "example", Scope: apiScopeAdmin, Hosted: &auth.HostedIdentity{Subject: "user_owner", ExpiresAt: now.Add(time.Hour)}, HostedRole: "owner", HostedMembership: "membership_owner", HostedPrincipal: "credential_owner"}}
	if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), "user_owner", "project_hidden")
	}); err != nil {
		t.Fatal(err)
	}
	visible, err := service.readOrganizationRankRecord(t.Context(), ownerScope)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(visible.ProjectIDs, []tracker.ProjectID{"project_last", "project_first"}) {
		t.Fatalf("visible order=%v", visible.ProjectIDs)
	}
	if _, err := service.updateOrganizationRankRecord(t.Context(), ownerScope, projectRankChange{ExpectedRevision: visible.Revision, ProjectIDs: []tracker.ProjectID{"project_first", "project_last"}}); err != nil {
		t.Fatal(err)
	}
	if err := repository.ReadTenant(t.Context(), "org_startup", corestore.Serving, func(r tenantstore.Reader) error {
		owner, err := organizationVisibleReader(r)
		if err != nil {
			return err
		}
		rows, err := owner.OrganizationRankProjects(t.Context(), 201)
		if err != nil {
			return err
		}
		var ids []tracker.ProjectID
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		if !slices.Equal(ids, []tracker.ProjectID{"project_first", "project_hidden", "project_last"}) {
			t.Fatalf("hidden slot moved: %v", ids)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.updateOrganizationRankRecord(t.Context(), ownerScope, projectRankChange{ExpectedRevision: visible.Revision + 1, ProjectIDs: []tracker.ProjectID{"project_first", "project_hidden", "project_last"}}); err == nil {
		t.Fatal("hidden project accepted in visible order")
	}
	if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		return w.DeleteAccessProjectGrant(t.Context(), "user_owner", "project_last")
	}); err != nil {
		t.Fatal(err)
	}
	visible, err = service.readOrganizationRankRecord(t.Context(), ownerScope)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(visible.ProjectIDs, []tracker.ProjectID{"project_first"}) {
		t.Fatalf("revoked project grant retained: %v", visible.ProjectIDs)
	}
	for _, tenant := range []struct {
		organization tracker.OrganizationID
		service      *Service
	}{{"org_startup", service}, {"org_second", secondService}} {
		for _, tc := range []struct {
			name         string
			keyGrants    []tracker.ProjectID
			memberGrants []tracker.ProjectID
			want         []tracker.ProjectID
		}{
			{name: "key_grant", keyGrants: []tracker.ProjectID{"project_first"}, want: []tracker.ProjectID{"project_first"}},
			{name: "member_grant", keyGrants: []tracker.ProjectID{"project_first", "project_last"}, memberGrants: []tracker.ProjectID{"project_last"}, want: []tracker.ProjectID{"project_last"}},
		} {
			t.Run(string(tenant.organization)+"/"+tc.name, func(t *testing.T) {
				key := seedSelectedServiceKey(t, repository, tenant.organization, "rank_"+tc.name, tc.keyGrants, tc.memberGrants, now)
				got, err := tenant.service.readOrganizationRankRecord(t.Context(), nativeScope{organization: tenant.organization, credential: key})
				if err != nil {
					t.Fatal(err)
				}
				if !slices.Equal(got.ProjectIDs, tc.want) {
					t.Fatalf("selected service key rank projects=%v want %v", got.ProjectIDs, tc.want)
				}
			})
		}
	}

	for _, tenant := range []string{"org_startup", "org_second"} {
		organization = tenant
		tenantBase := "/organizations/" + tenant + "/api/v2/organizations/" + tenant
		newer := map[string]any{"idempotency_key": "rename-newer", "name": "Newer " + tenant}
		requireNativeStatus(t, request(http.MethodPut, tenantBase+"/name", newer), http.StatusOK)
		operation := "PUT /api/v2/organizations/" + tenant + "/name"
		var original receiptstore.Record
		if err := repository.ReadTenant(t.Context(), tracker.OrganizationID(tenant), corestore.Serving, func(r tenantstore.Reader) error {
			var err error
			original, err = r.Receipt(t.Context(), "credential_owner", operation, "rename")
			if err != nil {
				return err
			}
			if original.ResponseReplayUntilNs != now.Add(receiptstore.ReplayRetention).UnixNano() {
				t.Fatalf("rename replay deadline=%d want %d", original.ResponseReplayUntilNs, now.Add(receiptstore.ReplayRetention).UnixNano())
			}
			rows, err := r.(receiptstore.RetentionReader).ReceiptExpiryPage(t.Context(), now.Add(receiptstore.ReplayRetention+time.Nanosecond), 50)
			if err == nil && len(rows) != 2 {
				t.Fatalf("eligible rename receipts=%d want 2", len(rows))
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		if err := repository.WriteTenant(t.Context(), tracker.OrganizationID(tenant), corestore.Serving, func(w tenantstore.Writer) error {
			expired, err := w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), original, now.Add(receiptstore.ReplayRetention))
			if err != nil {
				return err
			}
			if expired {
				t.Fatal("receipt expired at inclusive boundary")
			}
			expired, err = w.(receiptstore.RetentionWriter).ExpireReceipt(t.Context(), original, now.Add(receiptstore.ReplayRetention+time.Nanosecond))
			if err == nil && !expired {
				t.Fatal("eligible rename receipt did not expire")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		replayBody := body
		if tenant == "org_second" {
			replayBody = map[string]any{"idempotency_key": "rename", "name": "Second example"}
		}
		requireNativeStatus(t, request(http.MethodPut, tenantBase+"/name", replayBody), http.StatusGone)
		if err := repository.ReadTenant(t.Context(), tracker.OrganizationID(tenant), corestore.Serving, func(r tenantstore.Reader) error {
			stored, err := r.Receipt(t.Context(), original.ActorID, operation, "rename")
			if err != nil {
				return err
			}
			if !stored.ResponseExpired || stored.RequestHash != original.RequestHash || stored.PayloadObjectKey != original.PayloadObjectKey || stored.PayloadSHA256 != original.PayloadSHA256 || stored.ResponseReplayUntilNs != original.ResponseReplayUntilNs {
				t.Fatal("expired replay changed tombstone or recreated payload")
			}
			if _, err := eventstore.NewContent(service.runDetailStore()).Receipt(t.Context(), stored); !errors.Is(err, receiptstore.ErrExpired) {
				t.Fatalf("expired content read: %v", err)
			}
			object, err := r.ArchiveObjectByReference(t.Context(), eventstore.Reference(original.PayloadObjectKey, original.PayloadSHA256, original.PayloadBytes, original.PayloadContentBytes))
			if err != nil {
				return err
			}
			var owners int
			if err := admin.QueryRowContext(t.Context(), `SELECT count(*) FROM archive_object_owners WHERE organization_id=$1 AND archive_id=$2`, tenant, object.ID).Scan(&owners); err != nil {
				return err
			}
			if owners != 0 {
				t.Fatalf("expired rename response owners=%d", owners)
			}
			org, err := r.AccessOrganization(t.Context())
			if err == nil && org.Name != "Newer "+tenant {
				t.Fatal("expired replay overwrote newer organization name")
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	organization = "org_startup"

	for _, tenant := range []string{"org_startup", "org_second"} {
		for _, operation := range []string{"name", "project-rank"} {
			t.Run("lock-order/"+tenant+"/"+operation, func(t *testing.T) {
				organization = tenant
				target := service
				if tenant == "org_second" {
					target = secondService
				}
				currentRepository := target.config.TenantRepository
				var beforeName string
				var beforeRevision int64
				var beforeIDs []tracker.ProjectID
				if err := currentRepository.ReadTenant(t.Context(), tracker.OrganizationID(tenant), corestore.Serving, func(r tenantstore.Reader) error {
					org, err := r.AccessOrganization(t.Context())
					if err != nil {
						return err
					}
					beforeName = org.Name
					owner, err := organizationVisibleReader(r)
					if err != nil {
						return err
					}
					beforeRevision, err = owner.OrganizationSchedulingRevision(t.Context())
					if err != nil {
						return err
					}
					rows, err := owner.OrganizationRankProjects(t.Context(), 201)
					for _, row := range rows {
						beforeIDs = append(beforeIDs, row.ID)
					}
					return err
				}); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				requestContext = ctx
				locked, release := make(chan struct{}), make(chan struct{})
				ownerDone, mutationDone := make(chan struct{}), make(chan struct{})
				attempted := make(chan struct{}, 1)
				var ownerErr error
				var result *httptest.ResponseRecorder
				started := false
				defer func() {
					cancel()
					<-ownerDone
					if started {
						<-mutationDone
					}
					target.config.TenantRepository = currentRepository
					requestContext = t.Context()
				}()
				go func() {
					defer close(ownerDone)
					ownerErr = currentRepository.WriteTenant(ctx, tracker.OrganizationID(tenant), corestore.Background, func(w tenantstore.Writer) error {
						if err := w.LockAccessOrganization(ctx); err != nil {
							return err
						}
						close(locked)
						select {
						case <-release:
						case <-ctx.Done():
							return ctx.Err()
						}
						member, err := w.AccessMember(ctx, "user_owner")
						if err != nil {
							return err
						}
						member.Role = "member"
						return w.UpdateAccessMember(ctx, member)
					})
				}()
				select {
				case <-locked:
				case <-ownerDone:
					t.Fatalf("organization lock: %v", ownerErr)
				}
				target.config.TenantRepository = organizationLockProbeRepository{Repository: currentRepository, attempted: attempted}
				payload := map[string]any{"idempotency_key": "lock-order-" + operation, "name": "Forbidden rename"}
				if operation == "project-rank" {
					payload = map[string]any{"expected_revision": beforeRevision, "project_ids": []tracker.ProjectID{"project_last", "project_hidden", "project_first"}}
				}
				started = true
				go func() {
					defer close(mutationDone)
					result = request(http.MethodPut, "/organizations/"+tenant+"/api/v2/organizations/"+tenant+"/"+operation, payload)
				}()
				select {
				case <-attempted:
				case <-mutationDone:
					t.Fatalf("mutation never attempted organization lock: %d %s", result.Code, result.Body.String())
				}
				probe, err := admin.BeginTx(ctx, nil)
				if err != nil {
					t.Fatal(err)
				}
				var memberRole string
				probeErr := probe.QueryRowContext(ctx, `SELECT role FROM hosted_members WHERE organization_id=$1 AND user_id='user_owner' FOR UPDATE NOWAIT`, tenant).Scan(&memberRole)
				rollbackErr := probe.Rollback()
				if rollbackErr != nil {
					t.Fatal(rollbackErr)
				}
				if probeErr != nil {
					var conflict *pq.Error
					if errors.As(probeErr, &conflict) && conflict.Code == "55P03" {
						t.Fatal("waiting mutator holds conflicting member SHARE before organization lock")
					}
					t.Fatalf("member NOWAIT probe: %v", probeErr)
				}
				if memberRole != "owner" {
					t.Fatalf("probe role=%s", memberRole)
				}
				close(release)
				<-ownerDone
				if ownerErr != nil {
					t.Fatal(ownerErr)
				}
				<-mutationDone
				requireNativeStatus(t, result, http.StatusForbidden)
				target.config.TenantRepository = currentRepository
				if err := currentRepository.ReadTenant(t.Context(), tracker.OrganizationID(tenant), corestore.Serving, func(r tenantstore.Reader) error {
					org, err := r.AccessOrganization(t.Context())
					if err != nil {
						return err
					}
					owner, err := organizationVisibleReader(r)
					if err != nil {
						return err
					}
					revision, err := owner.OrganizationSchedulingRevision(t.Context())
					if err != nil {
						return err
					}
					rows, err := owner.OrganizationRankProjects(t.Context(), 201)
					if err != nil {
						return err
					}
					var ids []tracker.ProjectID
					for _, row := range rows {
						ids = append(ids, row.ID)
					}
					if org.Name != beforeName || revision != beforeRevision || !slices.Equal(ids, beforeIDs) {
						return fmt.Errorf("denied mutation changed organization name or rank")
					}
					member, err := r.AccessMember(t.Context(), "user_owner")
					if err != nil {
						return err
					}
					if member.Role != "member" {
						return fmt.Errorf("canonical demotion did not persist")
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := currentRepository.WriteTenant(t.Context(), tracker.OrganizationID(tenant), corestore.Serving, func(w tenantstore.Writer) error {
					if err := w.LockAccessOrganization(t.Context()); err != nil {
						return err
					}
					member, err := w.AccessMember(t.Context(), "user_owner")
					if err != nil {
						return err
					}
					member.Role = "owner"
					return w.UpdateAccessMember(t.Context(), member)
				}); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
	organization = "org_startup"
	requestContext = t.Context()

	if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error {
		member, err := w.AccessMember(t.Context(), "user_owner")
		if err != nil {
			return err
		}
		member.Role = "member"
		return w.UpdateAccessMember(t.Context(), member)
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.updateOrganizationRankRecord(t.Context(), ownerScope, projectRankChange{ExpectedRevision: visible.Revision, ProjectIDs: visible.ProjectIDs}); err == nil {
		t.Fatal("stale owner role authorized rank write")
	}
	if _, err := service.renameOrganizationRecord(t.Context(), ownerScope, renameOrganizationRequest{Mutation: tracker.Mutation{IdempotencyKey: "rename"}, organizationNameSettings: organizationNameSettings{Name: "Renamed example"}}); err == nil {
		t.Fatal("stale owner role authorized rename replay")
	}

	if err := repository.WriteTenant(t.Context(), "org_startup", corestore.Serving, func(w tenantstore.Writer) error { return w.RevokeAccessMember(t.Context(), "user_owner", now) }); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/name", "/project-rank"} {
		requireNativeStatus(t, request(http.MethodGet, base+path, nil), http.StatusForbidden)
	}
	requireNativeStatus(t, request(http.MethodPut, base+"/name", body), http.StatusForbidden)
}

type organizationLockProbeRepository struct {
	tenantstore.Repository
	attempted chan<- struct{}
}

func (r organizationLockProbeRepository) WriteTenant(ctx context.Context, organization tracker.OrganizationID, workload corestore.Workload, apply func(tenantstore.Writer) error) error {
	return r.Repository.WriteTenant(ctx, organization, workload, func(w tenantstore.Writer) error {
		owner, err := organizationVisibleWriter(w)
		if err != nil {
			return err
		}
		return apply(organizationLockProbeWriter{Writer: w, OrganizationWriter: owner, attempted: r.attempted})
	})
}

type organizationLockProbeWriter struct {
	tenantstore.Writer
	projectstore.OrganizationWriter
	attempted chan<- struct{}
}

func (w organizationLockProbeWriter) LockAccessOrganization(ctx context.Context) error {
	select {
	case w.attempted <- struct{}{}:
	default:
	}
	return w.Writer.LockAccessOrganization(ctx)
}
