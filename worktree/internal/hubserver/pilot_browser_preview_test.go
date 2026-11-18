//go:build detent_hostedpreview

package hubserver

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/digitaldrywood/detent/internal/artifact"
	"github.com/digitaldrywood/detent/internal/tracker"
	"github.com/digitaldrywood/detent/internal/web/templates"
)

func TestPilotBrowserPreview(t *testing.T) {
	t.Parallel()
	manifest := os.Getenv("DETENT_PILOT_BROWSER_MANIFEST")
	f, issue, change := pilotHostedJourney(t)
	versions := pilotReviewVersions(t, f)
	pilotExpireRunners(t, f)
	item := tracker.NativeWorkItemID(strings.Split(issue, "/")[8])
	info := map[string]string{"login": f.server.URL + "/login", "owner": f.server.URL + "/__preview/account/owner", "organization": f.server.URL + "/organization", "human_review": f.server.URL + "/projects/" + f.project, "automatic": f.server.URL + "/projects/" + f.privateProject, "issue": f.server.URL + templates.NativeIssuePath(f.project, item), "change": f.server.URL + templates.ChangePath(f.project, item, change[strings.LastIndex(change, "/")+1:]), "issue_api": f.server.URL + issue, "change_api": f.server.URL + change, "usage": f.server.URL + "/organization/plan", "stop": f.server.URL + "/__preview/stop"}
	for _, version := range versions {
		key := "automatic_change"
		if version.human {
			key = "human_change"
		}
		parts := strings.Split(version.path, "/")
		info[key] = f.server.URL + templates.ChangePath(parts[6], tracker.NativeWorkItemID(parts[8]), parts[10])
		info[key+"_api"] = f.server.URL + version.path
	}
	pilotBrowserWait(t, f, manifest, info)
}

func TestPilotOrganizationBrowserPreview(t *testing.T) {
	t.Parallel()
	manifest := os.Getenv("DETENT_PILOT_ORGANIZATION_MANIFEST")
	f := newBrowserHostedOrganizationFixture(t, false, artifact.NewID("org"))
	info := map[string]string{"login": f.server.URL + "/login", "organization": f.server.URL + "/organization", "owner": f.server.URL + "/__preview/account/owner", "invitee": f.server.URL + "/__preview/account/invitee", "stop": f.server.URL + "/__preview/stop"}
	info["invitation_mailbox"] = f.server.URL + "/__preview/invitations"
	pilotBrowserWait(t, f, manifest, info)
}

func pilotBrowserWait(t *testing.T, f *browserHostedFixture, manifest string, info map[string]string) {
	t.Helper()
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(manifest) {
		t.Fatal("browser manifest requires an absolute path")
	}
	if err := os.WriteFile(manifest, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Pilot browser fixture: %s", f.server.URL)
	timer := time.NewTimer(15 * time.Minute)
	defer timer.Stop()
	select {
	case <-f.stop:
	case <-timer.C:
		t.Fatal("pilot browser fixture was not stopped")
	case <-t.Context().Done():
	}
}
