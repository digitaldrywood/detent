package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/digitaldrywood/detent/internal/config/configdoc"
)

func main() {
	root := flag.String("root", ".", "repository root")
	check := flag.Bool("check", false, "check generated files without writing them")
	flag.Parse()

	absoluteRoot, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := configdoc.Generate(absoluteRoot, *check); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
