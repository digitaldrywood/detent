package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"
)

func TestUnmappedSourceColumnFailsCommand(t *testing.T) {
	files, manifest := routingFixture(t)
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	files["tools/storageinventory/routing.json"] = &fstest.MapFile{Data: data}
	files["internal/storagecatalog/targets/v1.json"] = &fstest.MapFile{Data: []byte(`{"version":"1","legacy_migrations":{},"migrations":[],"tables":{}}`)}
	files["tools/storageinventory/mappings/v1.json"] = &fstest.MapFile{Data: []byte(`{"version":"1","tables":{"hub/projects":{"family":"core","disposition":"target","contract":"Preserve project identity","organization":"binding","columns":{"id":{"source":"[0,\"id\",\"TEXT\",1,null,1,0]","disposition":"column","codec":"text","null":"reject","empty":"reject","targets":[{"table":"public.projects","column":"id","type":"text"}]}}}}}`)}
	for _, name := range []string{"internal/hubserver/testdata/hub", "internal/hubserver/testdata/metrics", "tools/storageinventory/sources/v1/registry", "tools/storageinventory/sources/v1/auth"} {
		files[name+"_schema.json"] = &fstest.MapFile{Data: []byte(`{}`)}
	}
	files["internal/hubserver/testdata/hub_schema.json"].Data = []byte(`{"column/projects/id":"[0,\"id\",\"TEXT\",1,null,1,0]"}`)
	root := t.TempDir()
	for name, entry := range files {
		if entry.Mode.IsDir() {
			if err := os.MkdirAll(filepath.Join(root, name), 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), entry.Data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-root", root}, &stdout, &stderr); code != 0 {
		t.Fatalf("mapped source: exit %d: %s", code, &stderr)
	}
	changed := []byte(`{"column/projects/id":"[0,\"id\",\"TEXT\",1,null,1,0]","column/projects/secret":"[1,\"secret\",\"BLOB\",0,null,0,0]"}`)
	if err := os.WriteFile(filepath.Join(root, "internal/hubserver/testdata/hub_schema.json"), changed, 0600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-root", root}, &stdout, &stderr); code != 1 {
		t.Fatalf("unmapped source column accepted: exit %d: %s %s", code, &stdout, &stderr)
	}
}
