package helper

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

const helperRan = "gotestreceipt helper ran"

var earlyMode = os.Getenv("GOTESTRECEIPT_EARLY")
var testDigestKey = []byte(strings.Repeat("k", 32))

func TestHelperProcess(t *testing.T) {
	fmt.Println(helperRan)
	if os.Getenv("GOTESTRECEIPT_HELPER_MODE") == "block" {
		select {}
	}
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "DETENT_GO_TEST") || strings.Contains(entry, string(testDigestKey)) {
			t.Fatalf("test binary received receipt session state %s", name)
		}
	}
	path := os.Getenv("GOTESTRECEIPT_HELPER_PATH")
	switch os.Getenv("GOTESTRECEIPT_HELPER_MODE") {
	case "exec":
		output, err := exec.Command("/bin/sh", "script.sh").Output()
		if err != nil || strings.TrimSpace(string(output)) != "pass" {
			t.Fatalf("script output %q: %v", output, err)
		}
		return
	case "early":
		if earlyMode != "" {
			t.Fatalf("early mode %q", earlyMode)
		}
		return
	case "mtime":
		info, err := os.Stat(path)
		if err != nil || info.ModTime().Year() < 2001 {
			t.Fatalf("stat %s: %v", path, err)
		}
		return
	case "remove":
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "pass" {
			t.Fatalf("input %q: %v", data, err)
		}
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		return
	case "length":
		if limit, err := strconv.Atoi(os.Getenv("GOTESTRECEIPT_HELPER_LIMIT")); err != nil || len(path) > limit {
			t.Fatalf("path %q exceeds the limit: %v", path, err)
		}
		return
	}
	data, err := os.ReadFile("input.txt")
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("GOTESTRECEIPT_HELPER_MODE") == "rewrite" {
		if err := os.WriteFile("input.txt", data, 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	if strings.TrimSpace(string(data)) != "pass" || os.Getenv("GOTESTRECEIPT_HELPER_MODE") == "fail" {
		t.Fatalf("input %q", data)
	}
}
