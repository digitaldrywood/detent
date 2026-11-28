package orgsettings_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/hubserver"
	"github.com/digitaldrywood/detent/internal/hubserver/attachmentstore"
	"github.com/digitaldrywood/detent/internal/hubserver/corestore"
	"github.com/digitaldrywood/detent/internal/hubserver/integration/hubfixture"
	"github.com/digitaldrywood/detent/internal/hubserver/orgsettings"
	"github.com/digitaldrywood/detent/internal/hubserver/tenantstore"
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationLogoLifecycle(t *testing.T) {
	t.Parallel()
	store := &memoryProjectIcons{objects: map[string][]byte{}}
	f := newSettingsFixture(t, hubserver.Config{Icons: store})
	other := f.otherOrganization(t, "Another organization")
	var content bytes.Buffer
	if err := png.Encode(&content, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	organizations := []tracker.OrganizationID{f.project.OrganizationID, other}
	for _, organization := range organizations {
		t.Run(string(organization), func(t *testing.T) {
			base := "/api/v2/organizations/" + string(organization)
			var previous string
			for i, remove := range []bool{false, false, true} {
				body := map[string]any{"expected_revision": strconv.Itoa(i + 1), "idempotency_key": fmt.Sprintf("logo-%d", i), "remove": remove}
				if !remove {
					body["content_base64"] = content.Bytes()
				}
				response := hubfixture.Request(t, f.service.Handler(), http.MethodPut, base+"/logo", testHubAdminToken, body)
				hubfixture.RequireStatus(t, response, http.StatusOK)
				var logo orgsettings.Logo
				hubfixture.Decode(t, response, &logo)
				if logo.Revision != tracker.Revision(i+2) || (logo.ImageKey == "") != remove {
					t.Fatalf("saved logo=%+v", logo)
				}
				if previous != "" {
					var deleted bool
					deleteErr := f.repository.ReadTenant(t.Context(), organization, corestore.Serving, func(r tenantstore.Reader) error {
						row, err := r.Attachment(t.Context(), "", previous)
						deleted = row.Metadata.DeletedAt != nil
						return err
					})
					if err := deleteErr; err != nil || !deleted {
						t.Fatalf("old logo was not deleted: %v", err)
					}
					hubfixture.RequireStatus(t, hubfixture.Request(t, f.service.Handler(), http.MethodGet, base+"/attachment-metadata/"+previous, testHubAdminToken, nil), http.StatusNotFound)
				}
				if !remove {
					stored := store.objects[string(organization)+"/"+logo.ImageKey]
					config, err := png.DecodeConfig(bytes.NewReader(stored))
					if err != nil || config.Width != 128 || config.Height != 128 || logo.ImageSHA256 == "" || logo.URL == "" {
						t.Fatalf("invalid logo=%+v image=%+v: %v", logo, config, err)
					}
					hubfixture.RequireStatus(t, hubfixture.Request(t, f.service.Handler(), http.MethodGet, base+"/attachment-metadata/"+logo.ImageKey, testHubAdminToken, nil), http.StatusOK)
					hubfixture.RequireStatus(t, hubfixture.Request(t, f.service.Handler(), http.MethodGet, base+"/logo-metadata", testHubAdminToken, nil), http.StatusOK)
					if err := f.repository.ReadTenant(t.Context(), organization, corestore.Background, func(r tenantstore.Reader) error {
						row, err := r.Attachment(t.Context(), "", logo.ImageKey)
						if err != nil {
							return err
						}
						if row.Retention != "organization" || row.Metadata.DeletedAt != nil {
							return fmt.Errorf("current logo lost organization retention")
						}
						candidates, err := r.(attachmentstore.ExpiryReader).AttachmentDeletionCandidates(t.Context(), row.Metadata.CreatedAt.Add(2*attachment.OrphanTTL), time.Time{}, 100)
						if err != nil {
							return err
						}
						if len(candidates) == 100 {
							return fmt.Errorf("logo expiry fixture exceeded bounded population")
						}
						for _, candidate := range candidates {
							if candidate.Metadata.ID == logo.ImageKey {
								return fmt.Errorf("current logo became an expired orphan")
							}
						}
						return nil
					}); err != nil {
						t.Fatal(err)
					}
					hubfixture.RequireStatus(t, hubfixture.Request(t, f.service.Handler(), http.MethodGet, base+"/logo-metadata", testHubAdminToken, nil), http.StatusOK)
				}
				replay := hubfixture.Request(t, f.service.Handler(), http.MethodPut, base+"/logo", testHubAdminToken, body)
				hubfixture.RequireStatus(t, replay, http.StatusOK)
				if !bytes.Equal(response.Body.Bytes(), replay.Body.Bytes()) {
					t.Fatal("logo replay changed")
				}
				previous = logo.ImageKey
			}
		})
	}
}

func TestOrganizationLogoRefusals(t *testing.T) {
	t.Parallel()
	var content bytes.Buffer
	if err := png.Encode(&content, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, revision string
		content        []byte
		manager        bool
		status         int
		failure        string
	}{
		{"accepted", "1", content.Bytes(), true, 200, ""},
		{"SVG", "1", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), true, 422, ""},
		{"stale", "2", content.Bytes(), true, 409, ""},
		{"non-manager", "1", content.Bytes(), false, 403, ""},
		{"storage quota", "1", content.Bytes(), true, 429, "quota"},
		{"revision changes during upload", "1", content.Bytes(), true, 409, "revision"},
		{"concurrent replay", "1", content.Bytes(), true, 200, "replay"},
		{"partially stored upload", "1", content.Bytes(), true, 500, "upload"},
		{"cancelled upload", "1", content.Bytes(), true, 500, "cancel"},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryProjectIcons{objects: map[string][]byte{}}
			var hosted *hostedFixture
			var f settingsFixture
			if test.failure == "quota" {
				hosted = newHostedFixture(t, hubserver.Config{Icons: store}, 0)
				f = hosted.settingsFixture
			} else {
				f = newSettingsFixture(t, hubserver.Config{Icons: store})
			}
			token := testHubAdminToken
			if !test.manager {
				token = f.token
			}
			path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/logo"
			body := map[string]any{"content_base64": test.content, "expected_revision": test.revision, "idempotency_key": "logo"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch test.failure {
			case "revision":
				store.afterPut = func(ctx context.Context) error {
					return f.repository.WriteTenant(ctx, f.project.OrganizationID, corestore.Serving, func(w tenantstore.Writer) error {
						logo, err := w.OrganizationLogo(ctx)
						if err != nil {
							return err
						}
						return w.UpdateOrganizationLogo(ctx, logo, logo.Revision)
					})
				}
			case "replay":
				store.afterPut = func(context.Context) error {
					store.afterPut = nil
					hubfixture.RequireStatus(t, hubfixture.Request(t, f.service.Handler(), http.MethodPut, path, token, body), http.StatusOK)
					return nil
				}
			case "upload":
				store.afterPut = func(context.Context) error { return errors.New("upload response lost") }
			case "cancel":
				store.afterPut = func(ctx context.Context) error {
					cancel()
					return ctx.Err()
				}
			}
			response := httptest.NewRecorder()
			request := hubfixture.NewRequest(t, http.MethodPut, path, token, body)
			if hosted != nil {
				request = hosted.request(t, "owner", http.MethodPut, path, body)
			}
			f.service.Handler().ServeHTTP(response, request.WithContext(ctx))
			hubfixture.RequireStatus(t, response, test.status)
			wantObjects := 0
			if test.status == 200 {
				wantObjects = 1
			}
			if len(store.objects) != wantObjects {
				t.Fatalf("stored objects=%d, want %d", len(store.objects), wantObjects)
			}
			var attachments int
			if err := f.admin.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE organization_id=$1", f.project.OrganizationID).Scan(&attachments); err != nil || attachments != wantObjects {
				t.Fatalf("attachment rows=%d, want %d: %v", attachments, wantObjects, err)
			}
			logo, err := orgsettings.New(orgsettings.Deps{Repository: f.repository}).ReadLogo(t.Context(), f.project.OrganizationID)
			if err != nil || (logo.ImageKey == "") != (wantObjects == 0) {
				t.Fatalf("logo=%+v: %v", logo, err)
			}
			if wantObjects != 0 && len(store.objects[string(f.project.OrganizationID)+"/"+logo.ImageKey]) == 0 {
				t.Fatal("committed logo object was deleted")
			}
		})
	}
}

func TestOrganizationLogoMCP(t *testing.T) {
	t.Parallel()
	store := &memoryProjectIcons{objects: map[string][]byte{}}
	f := newSettingsFixture(t, hubserver.Config{Icons: store})
	img := image.NewNRGBA(image.Rect(0, 0, 128, 128))
	state := uint32(1234567)
	for i := range img.Pix {
		state ^= state << 13
		state ^= state >> 17
		state ^= state << 5
		img.Pix[i] = byte(state)
	}
	var content bytes.Buffer
	if err := png.Encode(&content, img); err != nil {
		t.Fatal(err)
	}
	path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/mcp"
	for i, remove := range []bool{false, true} {
		args := map[string]any{"organization_id": f.project.OrganizationID, "remove": remove, "expected_revision": strconv.Itoa(i + 1), "request_id": fmt.Sprintf("logo-tool-%d", i)}
		if !remove {
			args["content_base64"] = content.Bytes()
		}
		response := hubfixture.WorkCall(t, f.service.Handler(), path, testHubAdminToken, "set_organization_logo", map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "tools/call", "params": map[string]any{"name": "set_organization_logo", "arguments": args, "_meta": json.RawMessage(fleetProtocolMeta)}})
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		hubfixture.Decode(t, response, &envelope)
		if response.Code != 200 || envelope.Result.IsError {
			t.Fatal(response.Body.String())
		}
		logo, err := orgsettings.New(orgsettings.Deps{Repository: f.repository}).ReadLogo(t.Context(), f.project.OrganizationID)
		if err != nil || logo.Revision != tracker.Revision(i+2) || (logo.ImageKey == "") != remove {
			t.Fatalf("logo=%+v %v", logo, err)
		}
	}
}

func TestHostedOrganizationLogoPermissions(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		role   string
		status int
	}{{"owner", 200}, {"admin", 200}, {"member", 403}, {"reporter", 403}, {"viewer", 403}} {
		t.Run(test.role, func(t *testing.T) {
			f := newHostedFixture(t, hubserver.Config{}, 64<<20)
			if err := f.provider.SetMembershipRole(t.Context(), f.members["owner"].Member.UserID, test.role); err != nil {
				t.Fatal(err)
			}
			if err := f.repository.WriteTenant(t.Context(), "org_browser_preview", corestore.Serving, func(w tenantstore.Writer) error {
				member, err := w.AccessMember(t.Context(), f.members["owner"].Member.UserID)
				if err != nil {
					return err
				}
				member.Role = test.role
				return w.UpdateAccessMember(t.Context(), member)
			}); err != nil {
				t.Fatal(err)
			}
			f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/logo", map[string]any{"remove": true, "expected_revision": "1", "idempotency_key": "logo"}, test.status)
			f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/logo", nil, 200)
			f.api(t, "owner", http.MethodGet, "/api/v2/organizations/org_other/"+"logo", nil, 404)
		})
	}
}
