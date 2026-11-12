package main

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestMakeCheckFastBuildHelper(t *testing.T) {
	if os.Getenv("DETENT_MAKE_BUILD_HELPER") != "1" {
		return
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Println("build ready")
	if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll("tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile("tmp/build-evidence", []byte(dir), 0o600); err != nil {
		t.Fatal(err)
	}
}
