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
	"github.com/digitaldrywood/detent/internal/auth"
	"github.com/digitaldrywood/detent/internal/cloudassert"
	"github.com/digitaldrywood/detent/internal/hubserver/accessstore"
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
	for _, org := range []string{"org_startup", "org_second"} {
		organization = org
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
		for _, serviceID := range []string{body["service_id"].(string), "service_missing"} {
			req := httptest.NewRequest(http.MethodPost, base+"/"+serviceID+"/authorize", strings.NewReader("["))
			assertionID, err := cloudassert.NewID()
			if err != nil {
				t.Fatal(err)
			}
			assertion, err := cloudassert.Sign(key, cloudassert.Claims{Issuer: "entry", Audience: org, Generation: 1, Kind: cloudassert.KindMachine, Method: http.MethodPost, Path: req.URL.Path, BodyDigest: cloudassert.BodyDigest([]byte("[")), IssuedAt: now, ExpiresAt: now.Add(20 * time.Second), ID: assertionID})
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
			want := http.StatusUnprocessableEntity
			if serviceID == "service_missing" {
				want = http.StatusNotFound
			}
			requireNativeStatus(t, response, want)
		}
		body["origin"] = "https://changed.example.test"
		requireNativeStatus(t, request(http.MethodPut, strings.Replace(base, "/artifact-services", "/onboarding/artifact-services", 1)+"/service_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", body), http.StatusConflict)
		body["idempotency_key"] = "missing-publisher"
		body["publisher_token_id"] = "credential_missing"
		requireNativeStatus(t, request(http.MethodPut, strings.Replace(base, "/artifact-services", "/onboarding/artifact-services", 1)+"/service_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", body), http.StatusNotFound)

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
