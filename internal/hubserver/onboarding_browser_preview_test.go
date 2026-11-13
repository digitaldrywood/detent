//go:build detent_hostedpreview

package hubserver

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestOnboardingBrowserPreview(t *testing.T) {
	t.Parallel()
	f := seedOnboardingBrowserJourney(t)
	interrupted := f.createProject(t, "Interrupted setup")
	requireNativeStatus(t, f.form(t, "owner", "/organization/grants", url.Values{"user": {"user_browser_owner"}, "project": {interrupted}, "write": {"true"}, "runner": {"true"}}), http.StatusSeeOther)
	info := map[string]string{"owner": f.server.URL + "/__preview/account/owner", "success": f.server.URL + "/projects/" + f.project, "auto_merge": f.server.URL + "/projects/" + f.privateProject, "interrupted": f.server.URL + "/projects/" + interrupted, "stop": f.server.URL + "/__preview/stop"}
	raw, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.TempDir(), "2194-browser.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("Onboarding browser fixture: %s", f.server.URL)
	timer := time.NewTimer(15 * time.Minute)
	defer timer.Stop()
	select {
	case <-f.stop:
	case <-timer.C:
	case <-t.Context().Done():
	}
}
