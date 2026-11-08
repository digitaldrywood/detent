package testsupport

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func isolatedPostgresBinaries(scratch string) error {
	if os.Getenv("DETENT_TEST_POSTGRES") != "binary" {
		return nil
	}
	dir := filepath.Join(scratch, "bin")
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	for name, options := range map[string]string{
		"initdb":   "--no-sync",
		"postgres": "-c fsync=off -c synchronous_commit=off -c full_page_writes=off",
	} {
		binary, err := exec.LookPath(name)
		if err != nil {
			return err
		}
		quoted := "'" + strings.ReplaceAll(binary, "'", "'\"'\"'") + "'"
		body := "#!/bin/sh\nexec " + quoted + " " + options + " \"$@\"\n"
		if name == "postgres" {
			body = "#!/bin/sh\nstop_postgres() {\ntrap '' INT TERM QUIT\nkill -INT \"$postgres_pid\" 2>/dev/null\nwait \"$postgres_pid\"\nexit \"$?\"\n}\ntrap stop_postgres INT TERM QUIT\n" + quoted + " " + options + " \"$@\" &\npostgres_pid=$!\nwait \"$postgres_pid\"\nexit \"$?\"\n"
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			return err
		}
	}
	return os.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}
