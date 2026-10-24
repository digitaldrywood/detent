package hubserver

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
	"github.com/digitaldrywood/detent/internal/tracker"
)

func TestOrganizationLogoLifecycle(t *testing.T) {
	t.Parallel()
	store := &memoryProjectIcons{objects: map[string][]byte{}}
	f := newDefaultNativeFixture(t, Config{Icons: store})
	other, err := f.service.createNativeOrganizationFor(t.Context(), "Another organization")
	if err != nil {
		t.Fatal(err)
	}
	var content bytes.Buffer
	if err := png.Encode(&content, image.NewNRGBA(image.Rect(0, 0, 64, 64))); err != nil {
		t.Fatal(err)
	}
	organizations := []tracker.OrganizationID{f.project.OrganizationID, other.ID}
	for _, organization := range organizations {
		t.Run(string(organization), func(t *testing.T) {
			base := "/api/v2/organizations/" + string(organization)
			var previous string
			for i, remove := range []bool{false, false, true} {
				body := map[string]any{"expected_revision": strconv.Itoa(i + 1), "idempotency_key": fmt.Sprintf("logo-%d", i), "remove": remove}
				if !remove {
					body["content_base64"] = content.Bytes()
				}
				response := performHubAPIRequest(t, f.service, http.MethodPut, base+"/logo", testHubAdminToken, body)
				requireNativeStatus(t, response, http.StatusOK)
				var logo organizationLogo
				decodeHubResponse(t, response, &logo)
				if logo.Revision != tracker.Revision(i+2) || (logo.ImageKey == "") != remove {
					t.Fatalf("saved logo=%+v", logo)
				}
				if previous != "" {
					var deleted bool
					if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT deleted_at IS NOT NULL FROM attachments WHERE id=?", previous).Scan(&deleted); err != nil || !deleted {
						t.Fatalf("old logo was not deleted: %v", err)
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, base+"/attachment-metadata/"+previous, testHubAdminToken, nil), http.StatusNotFound)
				}
				if !remove {
					stored := store.objects[string(organization)+"/"+logo.ImageKey]
					config, err := png.DecodeConfig(bytes.NewReader(stored))
					if err != nil || config.Width != 128 || config.Height != 128 || logo.ImageSHA256 == "" || logo.URL == "" {
						t.Fatalf("invalid logo=%+v image=%+v: %v", logo, config, err)
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, base+"/attachment-metadata/"+logo.ImageKey, testHubAdminToken, nil), http.StatusOK)
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, base+"/logo-metadata", testHubAdminToken, nil), http.StatusOK)
					if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE attachments SET created_at=? WHERE id=?", formatHubTime(time.Now().Add(-2*attachment.OrphanTTL)), logo.ImageKey); err != nil {
						t.Fatal(err)
					}
					if err := f.service.expireNativeRecords(t.Context(), time.Now()); err != nil {
						t.Fatal(err)
					}
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodGet, base+"/logo-metadata", testHubAdminToken, nil), http.StatusOK)
				}
				replay := performHubAPIRequest(t, f.service, http.MethodPut, base+"/logo", testHubAdminToken, body)
				requireNativeStatus(t, replay, http.StatusOK)
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
			f := newDefaultNativeFixture(t, Config{Icons: store})
			token := testHubAdminToken
			if !test.manager {
				token = f.token
			}
			path := "/api/v2/organizations/" + string(f.project.OrganizationID) + "/logo"
			body := map[string]any{"content_base64": test.content, "expected_revision": test.revision, "idempotency_key": "logo"}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			switch test.failure {
			case "quota":
				plans := pilotHostedPlans()
				plans.Plans[0].Allowances["collaboration_bytes"] = 0
				f.service.database.hostedOrganization = f.project.OrganizationID
				if err := f.service.database.configureHostedPlans(t.Context(), &HostedConfig{Plans: &plans}); err != nil {
					t.Fatal(err)
				}
			case "revision":
				store.afterPut = func(context.Context) error {
					_, err := f.service.database.db.ExecContext(t.Context(), "UPDATE organizations SET logo_revision=logo_revision+1 WHERE id=?", f.project.OrganizationID)
					return err
				}
			case "replay":
				store.afterPut = func(context.Context) error {
					store.afterPut = nil
					requireNativeStatus(t, performHubAPIRequest(t, f.service, http.MethodPut, path, token, body), http.StatusOK)
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
			f.service.Handler().ServeHTTP(response, newHubAPIRequest(t, http.MethodPut, path, token, body).WithContext(ctx))
			requireNativeStatus(t, response, test.status)
			wantObjects := 0
			if test.status == 200 {
				wantObjects = 1
			}
			if len(store.objects) != wantObjects {
				t.Fatalf("stored objects=%d, want %d", len(store.objects), wantObjects)
			}
			var attachments int
			if err := f.service.database.reader.QueryRowContext(t.Context(), "SELECT count(*) FROM attachments WHERE organization_id=?", f.project.OrganizationID).Scan(&attachments); err != nil || attachments != wantObjects {
				t.Fatalf("attachment rows=%d, want %d: %v", attachments, wantObjects, err)
			}
			logo, err := readOrganizationLogo(t.Context(), f.service.database.reader, f.project.OrganizationID)
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
	f := newDefaultNativeFixture(t, Config{Icons: store})
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
		response := performHubWorkCall(t, f.service, path, testHubAdminToken, "set_organization_logo", map[string]any{"jsonrpc": "2.0", "id": i + 1, "method": "tools/call", "params": map[string]any{"name": "set_organization_logo", "arguments": args, "_meta": json.RawMessage(fleetProtocolMeta)}})
		var envelope struct {
			Result struct {
				IsError bool `json:"isError"`
			} `json:"result"`
		}
		decodeHubResponse(t, response, &envelope)
		if response.Code != 200 || envelope.Result.IsError {
			t.Fatal(response.Body.String())
		}
		logo, err := readOrganizationLogo(t.Context(), f.service.database.reader, f.project.OrganizationID)
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
			f := newBrowserHostedFixtureServing(t, true, "org_browser_preview", false)
			if err := f.provider.SetMembershipRole(t.Context(), "membership_user_browser_owner", test.role); err != nil {
				t.Fatal(err)
			}
			if _, err := f.service.database.db.ExecContext(t.Context(), "UPDATE hosted_members SET role=? WHERE user_id='user_browser_owner'", test.role); err != nil {
				t.Fatal(err)
			}
			f.api(t, "owner", http.MethodPut, browserHostedOrganizationBase+"/logo", map[string]any{"remove": true, "expected_revision": "1", "idempotency_key": "logo"}, test.status)
			f.api(t, "viewer", http.MethodGet, browserHostedOrganizationBase+"/logo", nil, 200)
			f.api(t, "owner", http.MethodGet, "/api/v2/organizations/org_other/"+"logo", nil, 404)
		})
	}
}
