package hubserver

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
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
	}{
		{"accepted", "1", content.Bytes(), true, 200},
		{"SVG", "1", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), true, 422},
		{"stale", "2", content.Bytes(), true, 409},
		{"non-manager", "1", content.Bytes(), false, 403},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := &memoryProjectIcons{objects: map[string][]byte{}}
			f := newDefaultNativeFixture(t, Config{Icons: store})
			token := testHubAdminToken
			if !test.manager {
				token = f.token
			}
			response := performHubAPIRequest(t, f.service, http.MethodPut, "/api/v2/organizations/"+string(f.project.OrganizationID)+"/logo", token, map[string]any{"content_base64": test.content, "expected_revision": test.revision, "idempotency_key": "logo"})
			requireNativeStatus(t, response, test.status)
			if test.status != 200 && len(store.objects) != 0 {
				t.Fatal("refused upload stored an object")
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
