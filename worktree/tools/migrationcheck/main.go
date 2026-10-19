package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

type migrationSchema struct {
	directory         string
	namedThrough      int64
	sequentialThrough int64
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("migrationcheck", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "repository root containing migration directories")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "migrationcheck accepts only -root")
		return 2
	}
	if err := checkMigrations(os.DirFS(*root), []migrationSchema{
		{"internal/store/migrations", 68, 68},
		{"internal/hubserver/migrations", 70, 71},
		{"internal/cloudentry/migrations/registry", 5, 0},
		{"internal/cloudentry/migrations/auth", 4, 0},
	}); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintln(stdout, "Migration versions are unique within each schema and new Hub/store filenames use UTC YYYYMMDDHHMMSS_name.sql timestamps.")
	return 0
}

func checkMigrations(root fs.FS, schemas []migrationSchema) error {
	var failures []error
	for _, schema := range schemas {
		directory := schema.directory
		files, err := fs.ReadDir(root, directory)
		if err != nil {
			failures = append(failures, fmt.Errorf("read migrations: %w", err))
			continue
		}
		versions := make(map[int64]string)
		for _, file := range files {
			if file.IsDir() || path.Ext(file.Name()) != ".sql" {
				continue
			}
			name := path.Join(directory, file.Name())
			prefix, suffix, ok := strings.Cut(file.Name(), "_")
			version, err := strconv.ParseInt(prefix, 10, 64)
			if !ok || err != nil || version <= 0 {
				failures = append(failures, fmt.Errorf("invalid migration version: %s", name))
				continue
			}
			if schema.sequentialThrough != 0 && version > schema.sequentialThrough {
				_, timestampErr := time.Parse("20060102150405", prefix)
				if len(prefix) != 14 || timestampErr != nil || suffix == ".sql" {
					failures = append(failures, fmt.Errorf("noncanonical migration filename: %s; use UTC YYYYMMDDHHMMSS_name.sql", name))
				}
			} else if canonical := fmt.Sprintf("%05d_migration.sql", version); version > schema.namedThrough && file.Name() != canonical {
				failures = append(failures, fmt.Errorf("noncanonical migration filename: %s; use %s", name, path.Join(directory, canonical)))
			}
			if previous, exists := versions[version]; exists {
				failures = append(failures, fmt.Errorf("duplicate migration version %d: %s and %s", version, previous, name))
			}
			versions[version] = name
		}
		if len(versions) == 0 {
			failures = append(failures, fmt.Errorf("no SQL migrations in %s", directory))
		}
	}
	return errors.Join(failures...)
}
