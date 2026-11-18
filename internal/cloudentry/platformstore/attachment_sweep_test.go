package platformstore

import (
	"errors"
	"testing"
)

func TestAttachmentInventoryResumesEachPrefix(t *testing.T) {
	db := template.Database(t)
	cfg := testConfig(t)
	first := repository(t, db, cfg)
	seedOrganizations(t, first)
	interrupted := errors.New("listing interrupted")
	for _, org := range []string{"org_first", "org_second"} {
		err := first.VisitAttachmentPrefixes(t.Context(), org, func(prefix, after string) (string, error) {
			if after != "" {
				t.Fatalf("%s %s initial cursor %q", org, prefix, after)
			}
			return "orgs/" + org + "/" + prefix + "first", nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	restarted := repository(t, db, cfg)
	for _, org := range []string{"org_second", "org_first"} {
		for _, stop := range []bool{true, false} {
			visited := 0
			err := restarted.VisitAttachmentPrefixes(t.Context(), org, func(prefix, after string) (string, error) {
				visited++
				if want := "orgs/" + org + "/" + prefix + "first"; after != want {
					t.Fatalf("cursor %q, want %q", after, want)
				}
				if stop && prefix == "attempt-diffs/" {
					return "", interrupted
				}
				return "", nil
			})
			if visited != 2 || (stop && !errors.Is(err, interrupted)) || (!stop && err != nil) {
				t.Fatalf("visited %d prefixes: %v", visited, err)
			}
		}
	}
}
