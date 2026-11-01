package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/digitaldrywood/detent/internal/storagecatalog"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("storageinventory", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "repository root")
	manifestPath := flags.String("manifest", "tools/storageinventory/routing.json", "explicit storage routing map")
	catalogPath := flags.String("catalog", "tools/storageinventory/mappings/v1.json", "versioned source column mappings")
	scan := flags.Bool("scan", false, "print discovered source inventory without assigning routes")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return 2
	}
	inventory, err := scanRepository(os.DirFS(*root))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if *scan {
		encoder := json.NewEncoder(stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(inventory); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	manifest, err := readManifest(os.DirFS(*root), *manifestPath)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if problems := routingProblems(inventory, manifest); len(problems) != 0 {
		for _, problem := range problems {
			fmt.Fprintln(stderr, problem)
		}
		return 1
	}
	catalog, err := storagecatalog.Read(os.DirFS(*root), *catalogPath)
	if err == nil {
		err = catalog.ValidateInventory(os.DirFS(*root))
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "Storage routing verified: %d tables, %d SQL owners, %d atomic groups\n", len(inventory.Tables), len(inventory.Owners), len(manifest.AtomicGroups))
	return 0
}
