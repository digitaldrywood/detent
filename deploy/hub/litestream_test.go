package hub

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func TestLitestreamProductionBoundsWALAndPreservesReplicas(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile("litestream-production.json")
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Databases []struct {
			Path          string `json:"path"`
			Dir           string `json:"dir"`
			Pattern       string `json:"pattern"`
			Recursive     bool   `json:"recursive"`
			Watch         bool   `json:"watch"`
			TruncatePages int    `json:"truncate-page-n"`
			MinCheckpoint *int   `json:"min-checkpoint-page-count"`
			Replica       struct {
				Type     string `json:"type"`
				Bucket   string `json:"bucket"`
				Path     string `json:"path"`
				Endpoint string `json:"endpoint"`
				Region   string `json:"region"`
			} `json:"replica"`
		} `json:"dbs"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		t.Fatal(err)
	}
	for i, database := range config.Databases {
		t.Run(database.Dir+database.Path, func(t *testing.T) {
			if database.TruncatePages < 1 || database.TruncatePages > 121359 {
				t.Errorf("database %d truncate-page-n = %d, want 1..121359", i, database.TruncatePages)
			}
			if database.MinCheckpoint != nil && (*database.MinCheckpoint < 1 || *database.MinCheckpoint > 1000) {
				t.Errorf("database %d min-checkpoint-page-count = %d, want default or 1..1000", i, *database.MinCheckpoint)
			}
			if database.Replica.Type != "s3" || database.Replica.Bucket != "drywood-detent-litestream" || database.Replica.Endpoint != "https://nyc3.digitaloceanspaces.com" || database.Replica.Region != "nyc3" {
				t.Errorf("database %d replication destination changed: %+v", i, database.Replica)
			}
		})
	}
	for _, test := range []struct {
		name, path, dir, replica string
	}{
		{name: "tenants", dir: "/var/lib/detent-hub/tenants", replica: "production/tenants"},
		{name: "registry", path: "/var/lib/detent-hub/cloud/registry.db", replica: "production/cloud/registry.db"},
		{name: "auth", path: "/var/lib/detent-hub/cloud/auth.db", replica: "production/cloud/auth.db"},
	} {
		t.Run(test.name, func(t *testing.T) {
			matches := 0
			for _, database := range config.Databases {
				if database.Path != test.path || database.Dir != test.dir {
					continue
				}
				matches++
				if database.Replica.Path != test.replica {
					t.Errorf("replica path = %q, want %q", database.Replica.Path, test.replica)
				}
				if test.dir != "" && (database.Pattern != "hub.db" || !database.Recursive || !database.Watch) {
					t.Error("tenant replication requires the recursive, watched hub.db pattern")
				}
			}
			if matches != 1 {
				t.Errorf("database entries = %d, want exactly one", matches)
			}
		})
	}
}
