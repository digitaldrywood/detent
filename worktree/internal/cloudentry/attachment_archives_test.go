package cloudentry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/entryauthority"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type archiveFailureTransport struct {
	next http.RoundTripper
	path string
}

func (f archiveFailureTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method == http.MethodPost && r.URL.Path == f.path {
		return nil, errors.New("fixture-transport-secret https://objects.example.test/?X-Amz-Signature=fixture-secret")
	}
	return f.next.RoundTrip(r)
}

func TestArchiveLegacyBindingSnapshot(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	f.service.config.attachmentTransport = store.transport
	for _, organization := range []string{"org_alpha", "org_beta"} {
		cfg := store.config()
		cfg.Endpoint = "https://1.1.1.1"
		f.service.config.Attachments = &cfg
		initial, err := f.service.currentArchiveBinding(t.Context(), organization)
		if err != nil || initial.ID != "" || initial.Revision != 0 {
			t.Fatalf("legacy binding=%+v error=%v", initial, err)
		}
		ref, _, err := attachment.PrepareArchive(organization, 1, strings.NewReader("legacy archive"))
		if err != nil {
			t.Fatal(err)
		}
		cfg.Endpoint, cfg.Bucket = "https://8.8.8.8", "replacement"
		current, err := f.service.currentArchiveBinding(t.Context(), organization)
		if err != nil || current.Endpoint != "https://1.1.1.1" || current.Bucket != "private" {
			t.Fatal("legacy binding reinterpreted hosted configuration")
		}
		legacy, err := f.service.archiveStorageForReference(t.Context(), organization, ref)
		if err != nil {
			t.Fatal(err)
		}
		read, err := legacy.PresignArchive(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := url.Parse(read.URL)
		if err != nil || parsed.Host != "1.1.1.1" || parsed.Path != "/private/"+ref.Key {
			t.Fatal("legacy reference moved to the replacement bucket")
		}
		next, err := f.service.saveArchiveBinding(t.Context(), attachment.StorageBinding{Organization: organization, Backend: "hosted", Endpoint: cfg.Endpoint, Region: cfg.Region, Bucket: cfg.Bucket, CredentialReference: "hosted"}, 0)
		if err != nil || next.ID == "" || next.Revision != 1 {
			t.Fatal("hosted rebinding failed")
		}
		ref.BindingID = next.ID
		replacement, err := f.service.archiveStorageForReference(t.Context(), organization, ref)
		if err != nil {
			t.Fatal(err)
		}
		read, err = replacement.PresignArchive(t.Context(), ref)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err = url.Parse(read.URL)
		if err != nil || parsed.Host != "8.8.8.8" || parsed.Path != "/replacement/"+ref.Key {
			t.Fatal("new reference did not use its binding")
		}
	}
}

func TestArchiveRoutesLifecycle(t *testing.T) {
	t.Parallel()
	var logs bytes.Buffer
	f := newEntryFixtureWithLogger(t, slog.New(slog.NewTextHandler(&logs, nil)))
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
<<<<<<< HEAD
	storageConfig := store.config()
	f.service.config.Attachments = &storageConfig
	if policy := f.service.contentSecurity(); !strings.Contains(policy, "connect-src 'self' "+storageConfig.Endpoint+" ") || strings.Contains(policy, "connect-src *") {
		t.Fatal("archive CSP does not restrict storage to its configured endpoint")
	}
=======
	f.service.config.attachmentTransport = store.transport
	revoked := false
	f.service.config.ArchiveCredentials = func(_ context.Context, org, reference string) (attachment.Credentials, error) {
		if revoked && org == "org_alpha" {
			return attachment.Credentials{}, errors.New("fixture-revoked-secret")
		}
		if reference != "fixture-reference-secret" {
			return attachment.Credentials{}, errors.New("unexpected reference")
		}
		return attachment.Credentials{AccessKeyID: store.config().AccessKeyID, SecretAccessKey: store.config().SecretAccessKey}, nil
	}

>>>>>>> b98d035bbb (fix: complete prj_6d4919bebd73446798e6cd807feda10e#1533)
	alice := newBrowser(t, f.service.Handler())
	content := strings.Repeat("fixture-content-secret archived detail\n", 1000)
	var alphaArchiveAccess string
	var previous attachment.ArchiveReference
	for _, org := range []string{"org_alpha", "org_beta"} {
		alice.login("/organizations/"+org+"/organization", "user_alice:p"+org)
		_, page := alice.get("/organizations/" + org + "/organization")
		csrf := csrfFrom(t, page)
		project := attachmentProject(t, alice, org)
		base := "/api/v2/organizations/" + org + "/projects/" + project
		headers := map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrf, "X-Detent-Archive-Schema": "1"}
		items := make([]tracker.NativeIssue, 2)
		for index := range items {
			input := tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: org + "-" + strconv.Itoa(index)}, Title: []string{"Execution detail", "Review result"}[index], State: "Backlog"}
			raw, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			response := attachmentRequest(t, alice, http.MethodPost, base+"/work-items", bytes.NewReader(raw), headers)
			if response.Code != http.StatusOK {
				t.Fatalf("create item=%d %s", response.Code, response.Body.String())
			}
			if err := json.Unmarshal(response.Body.Bytes(), &items[index]); err != nil {
				t.Fatal(err)
			}
		}
		db, err := sql.Open("sqlite", f.fixtures[org].path+"?_pragma=foreign_keys(1)")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := db.Close(); err != nil {
				t.Error(err)
			}
		})
		usage := func() int64 {
			t.Helper()
			var used int64
			if err := db.QueryRowContext(t.Context(), "SELECT value FROM hosted_usage_counters WHERE name='collaboration_bytes'").Scan(&used); err != nil {
				t.Fatal(err)
			}
			return used
		}
		count := func(query string) int {
			t.Helper()
			var n int
			if err := db.QueryRowContext(t.Context(), query).Scan(&n); err != nil {
				t.Fatal(err)
			}
			return n
		}
		storedBytes := func() int64 {
			t.Helper()
			store.mu.Lock()
			defer store.mu.Unlock()
			var total int64
			for key, object := range store.objects {
				if strings.HasPrefix(key, "orgs/"+org+"/blobs/") {
					total += int64(len(object.content))
				}
			}
			return total
		}
		before := usage()
		if previous.Key != "" {
			response := attachmentRequest(t, alice, http.MethodGet, base+"/work-items/"+string(items[0].WorkItemID)+"/archives/"+previous.SHA256+"/access", nil, nil)
			if response.Code != http.StatusNotFound {
				t.Fatal("foreign organization archive was readable")
			}
		}
		target := base + "/work-items/" + string(items[0].WorkItemID) + "/archives"
		if org == "org_alpha" {
			store.mu.Lock()
			store.failPutResponse = true
			store.mu.Unlock()
			response := attachmentRequest(t, alice, http.MethodPost, target, strings.NewReader(content), headers)
			if response.Code != http.StatusBadGateway || count("SELECT count(*) FROM attachment_archive_references") != 0 || storedBytes() == 0 || usage()-before != storedBytes() {
				t.Fatalf("unpublished archive escaped quota accounting: status=%d charged=%d stored=%d %s", response.Code, usage()-before, storedBytes(), response.Body.String())
			}
			store.mu.Lock()
			store.failPutResponse = false
			store.mu.Unlock()
			original := f.service.config.transport
			metadataPath := base + "/attachment-metadata/archives/" + string(items[0].WorkItemID)
			f.service.config.transport = func(o Organization) (http.RoundTripper, error) {
				next, err := original(o)
				return archiveFailureTransport{next: next, path: metadataPath}, err
			}
			response = attachmentRequest(t, alice, http.MethodPost, target, strings.NewReader(content), headers)
			if response.Code != http.StatusBadGateway || count("SELECT count(*) FROM attachment_archive_references") != 0 || usage()-before != storedBytes() {
				t.Fatalf("precommit interruption lost accounting or published a reference: %d %s", response.Code, response.Body.String())
			}
			f.service.config.transport = original
			storage, err = attachment.NewStorage(t.Context(), store.config(), store.transport)
			if err != nil {
				t.Fatal(err)
			}
			f.service.attachments = storage
		}
		puts := 0
		for _, request := range store.requests() {
			if strings.HasPrefix(request, "PUT orgs/") {
				puts++
			}
		}
		var ref attachment.ArchiveReference
		for index, item := range items {
			target = base + "/work-items/" + string(item.WorkItemID) + "/archives"
			response := attachmentRequest(t, alice, http.MethodPost, target, strings.NewReader(content), headers)
			if response.Code != http.StatusCreated {
				t.Fatalf("upload=%d %s", response.Code, response.Body.String())
			}
			if err := json.Unmarshal(response.Body.Bytes(), &ref); err != nil || ref.Validate(org) != nil {
				t.Fatal("invalid archive reference")
			}
			if strings.Contains(response.Body.String(), "fixture-content-secret") || strings.Contains(response.Body.String(), "X-Amz-") {
				t.Fatal("upload leaked archive data or credentials")
			}
			if index == 0 {
				unreferenced := attachmentRequest(t, alice, http.MethodGet, base+"/work-items/"+string(items[1].WorkItemID)+"/archives/"+ref.SHA256+"/access", nil, nil)
				if unreferenced.Code != http.StatusNotFound {
					t.Fatal("archive escaped its work-item references")
				}
			}
			read := attachmentRequest(t, alice, http.MethodGet, target+"/"+ref.SHA256+"/access", nil, nil)
			if read.Code != http.StatusOK || read.Header().Get("Cache-Control") != "private, no-store" {
				t.Fatalf("read=%d %s", read.Code, read.Body.String())
			}
			var access attachment.ArchiveRead
			if err := json.Unmarshal(read.Body.Bytes(), &access); err != nil || access.ArchiveReference != ref || access.URL == "" || time.Until(access.ExpiresAt) > attachment.ArchiveReadLifetime || time.Until(access.ExpiresAt) < attachment.ArchiveReadLifetime-time.Minute {
				t.Fatal("invalid archive access")
			}
			if strings.Contains(read.Body.String(), "fixture-content-secret") || strings.Contains(read.Body.String(), store.config().SecretAccessKey) {
				t.Fatal("archive response leaked secret")
			}
		}
		if usage()-before != ref.CompressedSize || count("SELECT count(*) FROM attachments WHERE kind='archive'") != 1 || count("SELECT count(*) FROM attachment_archive_references") != 2 {
			t.Fatal("archive bytes were not metered once per organization")
		}
		if org == "org_alpha" {
			for _, request := range store.requests() {
				if strings.HasPrefix(request, "PUT orgs/") {
					puts--
				}
			}
			if puts != 0 {
				t.Fatal("restart reuploaded existing archive")
			}
		}
		missing := attachmentRequest(t, alice, http.MethodGet, base+"/work-items/wi_missing/archives/"+ref.SHA256+"/access", nil, nil)
		if missing.Code != http.StatusNotFound {
			t.Fatalf("missing item read=%d", missing.Code)
		}
		var archiveID string
		if err := db.QueryRowContext(t.Context(), "SELECT id FROM attachments WHERE kind='archive'").Scan(&archiveID); err != nil {
			t.Fatal(err)
		}
		for _, method := range []string{http.MethodGet, http.MethodDelete} {
			response := attachmentRequest(t, alice, method, base+"/attachments/"+archiveID, nil, headers)
			if response.Code != http.StatusNotFound {
				t.Fatalf("archive reachable through attachment %s: %d", method, response.Code)
			}
		}
		if _, err := db.ExecContext(t.Context(), "UPDATE attachments SET size=size+1 WHERE id=?", archiveID); err == nil {
			t.Fatal("archive metadata is mutable")
		}
		if org == "org_alpha" {
			response := attachmentRequest(t, alice, http.MethodPost, target, strings.NewReader("different archived content"), headers)
			var second attachment.ArchiveReference
			if response.Code != http.StatusCreated || json.Unmarshal(response.Body.Bytes(), &second) != nil || second.Validate(org) != nil || second.Key == ref.Key {
				t.Fatalf("second archive=%d %s", response.Code, response.Body.String())
			}
			if count("SELECT count(*) FROM attachments WHERE kind='archive'") != 2 || usage()-before != ref.CompressedSize+second.CompressedSize {
				t.Fatal("distinct archives did not receive separate identities and storage usage")
			}
			for _, archive := range []attachment.ArchiveReference{ref, second} {
				read := attachmentRequest(t, alice, http.MethodGet, target+"/"+archive.SHA256+"/access", nil, nil)
				var access attachment.ArchiveRead
				if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &access) != nil || access.ArchiveReference != archive {
					t.Fatal("distinct archives are not both usable")
				}
			}
		}
		for _, item := range items {
			if _, err := db.ExecContext(t.Context(), `INSERT INTO issue_archive_sections(organization_id,project_id,work_item_id,archive_revision,section_id,attachment_id) SELECT organization_id,project_id,work_item_id,?, ?,attachment_id FROM attachment_archive_references WHERE organization_id=? AND project_id=? AND work_item_id=? AND attachment_id=?`, 1, "issue", org, project, item.WorkItemID, archiveID); err != nil {
				t.Fatal(err)
			}
			access := base + "/work-items/" + string(item.WorkItemID) + "/archives/" + ref.SHA256 + "/access"
			for _, test := range []struct {
				query  string
				status int
			}{
				{"?section=issue&revision=1", http.StatusOK},
				{"?section=issue&revision=2", http.StatusNotFound},
				{"", http.StatusNotFound},
			} {
				read := attachmentRequest(t, alice, http.MethodGet, access+test.query, nil, nil)
				if read.Code != test.status {
					t.Fatalf("section access %s: %d %s", test.query, read.Code, read.Body.String())
				}
			}
		}
		if _, err := db.ExecContext(t.Context(), "DELETE FROM attachment_archive_references WHERE work_item_id=?", items[0].WorkItemID); err != nil {
			t.Fatal(err)
		}
		organization, err := f.service.readyOrganization(t.Context(), org)
		if err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		object := store.objects[ref.Key]
		object.created = time.Now().Add(-2 * attachment.OrphanTTL)
		store.objects[ref.Key] = object
		store.mu.Unlock()
		if err := f.service.sweepOrganizationAttachments(t.Context(), organization); err != nil {
			t.Fatal(err)
		}
		store.mu.Lock()
		_, exists := store.objects[ref.Key]
		store.mu.Unlock()
		if !exists {
			t.Fatal("unreferenced archive expired")
		}
		if previous.Key != "" && (ref.Key == previous.Key || ref.SHA256 != previous.SHA256) {
			t.Fatal("organization deduplication boundary failed")
		}
		previous = ref

		originalUsage := usage()
		references := []attachment.ArchiveReference{ref}
		bindingTarget := "/api/cloud/organizations/" + org + "/archive-storage"
		archiveTarget := base + "/work-items/" + string(items[1].WorkItemID) + "/archives"
		for revision, endpoint := range []string{"https://1.1.1.1", "https://8.8.8.8"} {
			raw, err := json.Marshal(map[string]any{"backend": "s3", "endpoint": endpoint, "region": "region", "bucket": "private", "prefix": "revision_" + strconv.Itoa(revision+1) + "/", "credential_reference": "fixture-reference-secret", "expected_revision": revision})
			if err != nil {
				t.Fatal(err)
			}
			bound := attachmentRequest(t, alice, http.MethodPut, bindingTarget, bytes.NewReader(raw), headers)
			var binding attachment.StorageBinding
			if bound.Code != http.StatusOK || json.Unmarshal(bound.Body.Bytes(), &binding) != nil || binding.Revision != int64(revision+1) || binding.Organization != org {
				t.Fatalf("bind=%d %s", bound.Code, bound.Body.String())
			}
			if strings.Contains(bound.Body.String(), "fixture-reference-secret") || strings.Contains(bound.Body.String(), store.config().SecretAccessKey) {
				t.Fatal("binding response leaked credentials")
			}
			stale := attachmentRequest(t, alice, http.MethodPut, bindingTarget, bytes.NewReader(raw), headers)
			if stale.Code != http.StatusConflict {
				t.Fatalf("stale binding=%d %s", stale.Code, stale.Body.String())
			}
			uploaded := attachmentRequest(t, alice, http.MethodPost, archiveTarget, strings.NewReader(content), headers)
			var current attachment.ArchiveReference
			if uploaded.Code != http.StatusCreated || json.Unmarshal(uploaded.Body.Bytes(), &current) != nil || current.BindingID != binding.ID || !current.CustomerOwned {
				t.Fatalf("bound upload=%d %s", uploaded.Code, uploaded.Body.String())
			}
			references = append(references, current)
			if org == "org_alpha" {
				alphaArchiveAccess = archiveTarget + "/" + current.SHA256 + "/access?binding=" + current.BindingID
			}
			for index, old := range references {
				read := attachmentRequest(t, alice, http.MethodGet, archiveTarget+"/"+old.SHA256+"/access?binding="+url.QueryEscape(old.BindingID), nil, nil)
				var access attachment.ArchiveRead
				if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &access) != nil || access.ArchiveReference != old {
					t.Fatalf("old binding lost: %d %s", read.Code, read.Body.String())
				}
				parsed, err := url.Parse(access.URL)
				if err != nil {
					t.Fatal(err)
				}
				prefix := ""
				if index > 0 {
					prefix = "revision_" + strconv.Itoa(index) + "/"
				}
				if parsed.Path != "/private/"+prefix+old.Key {
					t.Fatalf("old reference reinterpreted: %s", parsed.Path)
				}
				if index > 0 && parsed.Scheme+"://"+parsed.Host != []string{"https://1.1.1.1", "https://8.8.8.8"}[index-1] {
					t.Fatal("old reference moved endpoint")
				}
				store.mu.Lock()
				_, exists := store.objects[prefix+old.Key]
				store.mu.Unlock()
				if !exists {
					t.Fatal("bound object is missing")
				}
			}
			if usage() != originalUsage {
				t.Fatalf("customer bytes charged: before=%d after=%d", originalUsage, usage())
			}
			page := attachmentRequest(t, alice, http.MethodGet, "/organizations/"+org+"/organization", nil, nil)
			if !strings.Contains(page.Header().Get("Content-Security-Policy"), endpoint) {
				t.Fatalf("validated archive endpoint missing from CSP: status=%d policy=%q location=%q", page.Code, page.Header().Get("Content-Security-Policy"), page.Header().Get("Location"))
			}
		}
	}
	store.mu.Lock()
	n := len(store.objects)
	store.mu.Unlock()
	if n != 7 {
		t.Fatalf("stored blobs=%d, want seven tenant-scoped blobs", n)
	}
	for _, secret := range []string{"fixture-content-secret", "fixture-transport-secret", "fixture-reference-secret", "fixture-revoked-secret", "X-Amz-Signature", store.config().SecretAccessKey} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("secret leaked into logs: %s", secret)
		}
	}
	revoked = true
	unavailable := attachmentRequest(t, alice, http.MethodGet, alphaArchiveAccess, nil, nil)
	if unavailable.Code != http.StatusBadGateway || !strings.Contains(unavailable.Body.String(), "archive_storage_unavailable") {
		t.Fatalf("archive failure lost history availability: %d %s", unavailable.Code, unavailable.Body.String())
	}
	revoked = false
	f.service.config.Allocation = &AllocationConfig{Launcher: &silentLauncher{running: map[string]bool{}}}
	if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state='deleting' WHERE id='org_alpha'"); err != nil {
		t.Fatal(err)
	}
	revoked = true
	if err := f.service.finishDeletion(t.Context(), "org_alpha"); err == nil {
		t.Fatal("revoked customer storage completed deletion")
	}
	organization, err := f.service.registry.Organization(t.Context(), "org_alpha")
	if err != nil || organization.State != "deleting" {
		t.Fatal("failed deletion lost pending state")
	}
	var pending int
	if err := f.service.registry.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_deletion_failures WHERE organization_id='org_alpha' AND error_code='credentials_or_endpoint_unavailable'").Scan(&pending); err != nil || pending != 2 {
		t.Fatalf("revoked receipts=%d error=%v", pending, err)
	}
	receipt := attachmentRequest(t, alice, http.MethodGet, "/api/cloud/organizations/org_alpha/archive-deletion", nil, nil)
	if receipt.Code != http.StatusOK || !strings.Contains(receipt.Body.String(), "credentials_or_endpoint_unavailable") || strings.Contains(receipt.Body.String(), "fixture-reference-secret") || !strings.Contains(receipt.Body.String(), `"bucket":"private"`) {
		t.Fatalf("receipt=%d %s", receipt.Code, receipt.Body.String())
	}
	revoked = false
	if err := f.service.finishDeletion(t.Context(), "org_alpha"); err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if len(store.objects) != 3 {
		t.Fatal("organization deletion failed to remove its archive prefix")
	}
	for key := range store.objects {
		if !strings.HasPrefix(key, "orgs/org_beta/") && !strings.HasPrefix(key, "revision_1/orgs/org_beta/") && !strings.HasPrefix(key, "revision_2/orgs/org_beta/") {
			t.Fatal("organization deletion crossed tenant boundary")
		}
	}
}

func TestTenantArchiveSectionReader(t *testing.T) {
	f := newEntryFixture(t)
	store := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), store.config(), store.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
	organization, err := f.service.readyOrganization(t.Context(), "org_alpha")
	if err != nil {
		t.Fatal(err)
	}
	section := attachment.ArchiveSection{SchemaVersion: 1, OrganizationID: organization.ID, ProjectID: "prj_example", WorkItemID: "wi_example", SectionID: "run_example", Tables: []attachment.ArchiveTable{}}
	raw, err := json.Marshal(section)
	if err != nil {
		t.Fatal(err)
	}
	ref, data, err := attachment.PrepareArchive(organization.ID, 1, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if err := attachment.StoreArchive(t.Context(), storage.(attachment.ArchiveStorage), organization.ID, ref, data); err != nil {
		t.Fatal(err)
	}
	reader := f.service.tenantArchiveReader(organization)
	for _, presign := range []bool{false, true} {
		result, err := reader(t.Context(), ref, presign)
		if err != nil || !bytes.Equal(result.Data, data) || (result.Access != nil) != presign {
			t.Fatalf("tenant section read presign=%v: %v", presign, err)
		}
		if result.Access != nil && (result.Access.ArchiveReference != ref || result.Access.ExpiresAt.After(time.Now().Add(attachment.ArchiveReadLifetime))) {
			t.Fatal("archive URL exceeds its reference or lifetime")
		}
	}
	foreign := ref
	foreign.Key, err = attachment.ArchiveKey("org_beta", ref.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reader(t.Context(), foreign, false); err == nil {
		t.Fatal("tenant read a foreign archive")
	}
	organization.Generation++
	if _, err := f.service.tenantArchiveReader(organization)(t.Context(), ref, false); err == nil {
		t.Fatal("stale tenant generation read archive bytes")
	}
}

func TestTenantContentStorage(t *testing.T) {
	t.Parallel()
	f := newEntryFixture(t)
	spaces := newSpacesFixture(t, false)
	storage, err := attachment.NewStorage(t.Context(), spaces.config(), spaces.transport)
	if err != nil {
		t.Fatal(err)
	}
	f.service.attachments = storage
	content := strings.Repeat("immutable source and review output\n", 10000)
	for _, org := range []string{"org_alpha", "org_beta"} {
		organization, err := f.service.readyOrganization(t.Context(), org)
		if err != nil {
			t.Fatal(err)
		}
		ref, data, err := attachment.PrepareArchive(org, 1, strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		request := entryauthority.ContentRequest{Organization: org, Reference: ref, Write: true, Data: data}
		for range 2 {
			if _, err := f.service.tenantContent(t.Context(), organization, request); err != nil {
				t.Fatal(err)
			}
		}
		request.Write = false
		request.Data = nil
		restored, err := f.service.tenantContent(t.Context(), organization, request)
		if err != nil || !bytes.Equal(restored, data) {
			t.Fatalf("immutable tenant content changed: %v", err)
		}
		foreign := organization
		foreign.ID = "org_other"
		if _, err := f.service.tenantContent(t.Context(), foreign, request); err == nil {
			t.Fatal("cross-org content access accepted")
		}
		stale := organization
		stale.Generation++
		if _, err := f.service.tenantContent(t.Context(), stale, request); err == nil {
			t.Fatal("stale allocation read accepted")
		}
	}
	spaces.mu.Lock()
	defer spaces.mu.Unlock()
	if len(spaces.objects) != 2 {
		t.Fatalf("stored %d objects, want one per org", len(spaces.objects))
	}
}
