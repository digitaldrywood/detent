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
	"sync"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
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

type archiveValidationTransport struct {
	next  http.RoundTripper
	check func(*http.Request) error
}

func (f archiveValidationTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if err := f.check(r); err != nil {
		return nil, err
	}
	return f.next.RoundTrip(r)
}

func TestArchiveBindingValidationIsolation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, stage, mutation string
		status, bindings      int
	}{
		{name: "lifecycle discovery", stage: "lifecycle", status: http.StatusOK, bindings: 1},
		{name: "versioning discovery", stage: "versioning", status: http.StatusOK, bindings: 1},
		{name: "organization deleted during discovery", stage: "versioning", mutation: "deleting", status: http.StatusServiceUnavailable},
		{name: "binding replaced during discovery", stage: "versioning", mutation: "rebind", status: http.StatusConflict, bindings: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEntryFixture(t)
			store := newSpacesFixture(t, false)
			f.service.config.Allocation = &AllocationConfig{Launcher: &silentLauncher{running: map[string]bool{}}}
			f.service.config.ArchiveCredentials = func(context.Context, string, string) (attachment.Credentials, error) {
				return attachment.Credentials{AccessKeyID: store.config().AccessKeyID, SecretAccessKey: store.config().SecretAccessKey}, nil
			}
			alice := newBrowser(t, f.service.Handler())
			alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			_, page := alice.get("/organizations/org_alpha/organization")
			if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state='deleting' WHERE id='org_beta'"); err != nil {
				t.Fatal(err)
			}
			checks := 0
			f.service.config.attachmentTransport = archiveValidationTransport{next: store.transport, check: func(r *http.Request) error {
				if !r.URL.Query().Has(test.stage) {
					return nil
				}
				checks++
				if err := f.service.finishDeletion(r.Context(), "org_beta"); err != nil {
					return err
				}
				switch test.mutation {
				case "deleting":
					_, err := f.service.registry.store.db.ExecContext(r.Context(), "UPDATE organizations SET state='deleting' WHERE id='org_alpha'")
					return err
				case "rebind":
					_, err := f.service.saveArchiveBinding(r.Context(), attachment.StorageBinding{Organization: "org_alpha", Backend: "s3", Endpoint: "https://8.8.8.8", Region: "region", Bucket: "private", Prefix: "winner/", CredentialReference: "reference"}, 0)
					return err
				}
				return nil
			}}
			raw := `{"backend":"s3","endpoint":"https://1.1.1.1","region":"region","bucket":"private","credential_reference":"reference","expected_revision":0}`
			response := attachmentRequest(t, alice, http.MethodPut, "/api/cloud/organizations/org_alpha/archive-storage", strings.NewReader(raw), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrfFrom(t, page)})
			if response.Code != test.status || checks != 1 {
				t.Fatalf("binding=%d checks=%d %s", response.Code, checks, response.Body.String())
			}
			organization, err := f.service.registry.Organization(t.Context(), "org_beta")
			if err != nil || organization.State != "deleted" {
				t.Fatalf("other organization deletion stalled: organization=%+v error=%v", organization, err)
			}
			var bindings int
			if err := f.service.registry.store.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM archive_storage_bindings WHERE organization_id='org_alpha'").Scan(&bindings); err != nil || bindings != test.bindings {
				t.Fatalf("bindings=%d error=%v", bindings, err)
			}
			if test.mutation == "rebind" {
				binding, err := f.service.currentArchiveBinding(t.Context(), "org_alpha")
				if err != nil || binding.Prefix != "winner/" {
					t.Fatal("stale discovery replaced the newer binding")
				}
			}
		})
	}
}

func TestArchiveDeletionIsolation(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, method, query string
		ready               bool
	}{
		{name: "versioning", method: http.MethodGet, query: "versioning"},
		{name: "listing", method: http.MethodGet, query: "list-type"},
		{name: "object deletion", method: http.MethodDelete},
		{name: "ready organization is fenced", ready: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newEntryFixture(t)
			store := newSpacesFixture(t, false)
			f.service.config.Allocation = &AllocationConfig{Launcher: &silentLauncher{running: map[string]bool{}}}
			f.service.config.ArchiveCredentials = func(context.Context, string, string) (attachment.Credentials, error) {
				return attachment.Credentials{AccessKeyID: store.config().AccessKeyID, SecretAccessKey: store.config().SecretAccessKey}, nil
			}
			if _, err := f.service.saveArchiveBinding(t.Context(), attachment.StorageBinding{Organization: "org_alpha", Backend: "s3", Endpoint: "https://1.1.1.1", Region: "region", Bucket: "private", CredentialReference: "reference"}, 0); err != nil {
				t.Fatal(err)
			}
			key := "orgs/org_alpha/blobs/sha256/aa/object"
			store.objects[key] = spacesObject{content: []byte("archive"), created: time.Now()}
			if !test.ready {
				if _, err := f.service.registry.store.db.ExecContext(t.Context(), "UPDATE organizations SET state='deleting' WHERE id IN ('org_alpha','org_beta')"); err != nil {
					t.Fatal(err)
				}
			}
			checks := 0
			f.service.config.attachmentTransport = archiveValidationTransport{next: store.transport, check: func(r *http.Request) error {
				if test.ready {
					t.Error("ready organization contacted object storage")
					return errors.New("organization still ready")
				}
				if r.Method != test.method || test.query != "" && !r.URL.Query().Has(test.query) {
					return nil
				}
				checks++
				if _, err := f.service.readyOrganization(r.Context(), "org_alpha"); err == nil {
					return errors.New("deleting organization accepts writes")
				}
				return f.service.finishDeletion(r.Context(), "org_beta")
			}}
			err := f.service.finishDeletion(t.Context(), "org_alpha")
			if test.ready {
				if err == nil {
					t.Fatal("ready organization deletion succeeded")
				}
				if _, exists := store.objects[key]; !exists {
					t.Fatal("ready organization archive was removed")
				}
				return
			}
			if err != nil || checks != 1 {
				t.Fatalf("deletion error=%v checks=%d", err, checks)
			}
			for _, id := range []string{"org_alpha", "org_beta"} {
				organization, err := f.service.registry.Organization(t.Context(), id)
				if err != nil || organization.State != "deleted" {
					t.Fatalf("organization=%+v error=%v", organization, err)
				}
			}
			if _, exists := store.objects[key]; exists {
				t.Fatal("archive was not erased")
			}
		})
	}
}

func TestArchiveUploadIsolation(t *testing.T) {
	t.Parallel()
	for _, stage := range []string{"initial read", "upload", "verification"} {
		t.Run(stage, func(t *testing.T) {
			f := newEntryFixture(t)
			store := newSpacesFixture(t, false)
			f.service.config.Allocation = &AllocationConfig{Launcher: &silentLauncher{running: map[string]bool{}}}
			f.service.config.ArchiveCredentials = func(context.Context, string, string) (attachment.Credentials, error) {
				return attachment.Credentials{AccessKeyID: store.config().AccessKeyID, SecretAccessKey: store.config().SecretAccessKey}, nil
			}
			if _, err := f.service.saveArchiveBinding(t.Context(), attachment.StorageBinding{Organization: "org_alpha", Backend: "s3", Endpoint: "https://1.1.1.1", Region: "region", Bucket: "private", CredentialReference: "reference"}, 0); err != nil {
				t.Fatal(err)
			}
			alice := newBrowser(t, f.service.Handler())
			alice.login("/organizations/org_alpha/organization", "user_alice:porg_alpha")
			_, page := alice.get("/organizations/org_alpha/organization")
			project := attachmentProject(t, alice, "org_alpha")
			base := "/api/v2/organizations/org_alpha/projects/" + project
			headers := map[string]string{"Content-Type": "application/json", "X-CSRF-Token": csrfFrom(t, page), "X-Detent-Archive-Schema": "1"}
			raw, err := json.Marshal(tracker.CreateIssue{Mutation: tracker.Mutation{IdempotencyKey: "upload_isolation"}, Title: "Archived detail", State: "Backlog"})
			if err != nil {
				t.Fatal(err)
			}
			created := attachmentRequest(t, alice, http.MethodPost, base+"/work-items", bytes.NewReader(raw), headers)
			var item tracker.NativeIssue
			if created.Code != http.StatusOK || json.Unmarshal(created.Body.Bytes(), &item) != nil {
				t.Fatalf("create item=%d %s", created.Code, created.Body.String())
			}
			entered, release := make(chan struct{}), make(chan struct{})
			releaseUpload := sync.OnceFunc(func() { close(release) })
			reads := 0
			f.service.config.attachmentTransport = archiveValidationTransport{next: store.transport, check: func(r *http.Request) error {
				if !strings.Contains(r.URL.Path, "/orgs/org_alpha/blobs/") {
					return nil
				}
				if r.Method == http.MethodGet {
					reads++
				}
				stall := stage == "initial read" && r.Method == http.MethodGet && reads == 1 || stage == "upload" && r.Method == http.MethodPut || stage == "verification" && r.Method == http.MethodGet && reads == 2
				if stall {
					close(entered)
					select {
					case <-release:
					case <-r.Context().Done():
						return r.Context().Err()
					}
				}
				return nil
			}}
			uploadFinished := make(chan struct{})
			var uploadStatus int
			target := base + "/work-items/" + string(item.WorkItemID) + "/archives"
			go func() {
				defer close(uploadFinished)
				uploadStatus = attachmentRequest(t, alice, http.MethodPost, target, strings.NewReader("archive content"), headers).Code
			}()
			defer func() { releaseUpload(); <-uploadFinished }()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("upload did not reach storage")
			}
			if _, err := f.service.registry.store.db.ExecContext(ctx, "UPDATE organizations SET state='deleting' WHERE id IN ('org_alpha','org_beta')"); err != nil {
				t.Fatal(err)
			}
			alphaFinished, betaFinished := make(chan struct{}), make(chan struct{})
			var alphaError, betaError error
			go func() { defer close(alphaFinished); alphaError = f.service.finishDeletion(t.Context(), "org_alpha") }()
			go func() { defer close(betaFinished); betaError = f.service.finishDeletion(t.Context(), "org_beta") }()
			defer func() { releaseUpload(); <-alphaFinished; <-betaFinished }()
			select {
			case <-betaFinished:
			case <-ctx.Done():
				t.Fatal("another organization's deletion stalled behind the upload")
			}
			if betaError != nil {
				t.Fatal(betaError)
			}
			if rejected := attachmentRequest(t, alice, http.MethodPost, target, strings.NewReader("late archive"), headers); rejected.Code != http.StatusNotFound {
				t.Fatalf("deleting organization accepted upload: %d", rejected.Code)
			}
			select {
			case <-alphaFinished:
				t.Fatal("same organization deletion did not drain its upload")
			default:
			}
			releaseUpload()
			<-uploadFinished
			<-alphaFinished
			if uploadStatus != http.StatusCreated || alphaError != nil {
				t.Fatalf("upload=%d deletion error=%v", uploadStatus, alphaError)
			}
			store.mu.Lock()
			defer store.mu.Unlock()
			for key := range store.objects {
				if strings.HasPrefix(key, "orgs/org_alpha/") {
					t.Fatalf("deletion retained completed upload %s", key)
				}
			}
		})
	}
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
