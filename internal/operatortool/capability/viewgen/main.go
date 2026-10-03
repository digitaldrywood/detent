// viewgen renders the review document or lists independently discovered source
// sites for fixture authors. It is not a Detent CLI subcommand or runtime gate.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/digitaldrywood/detent/internal/operatortool/capability"
)

func main() {
	root := flag.String("root", "../../..", "repository root (default: from the capability package)")
	sources := flag.Bool("sources", false, "print discovered source sites instead of rendering documentation")
	flag.Parse()
	if err := generate(*root, *sources, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func generate(root string, sources bool, stdout io.Writer) error {
	var err error
	if sources {
		var sites []capability.Candidate
		sites, err = capability.Discover(os.DirFS(root))
		if err == nil {
			encoder := json.NewEncoder(stdout)
			encoder.SetIndent("", "  ")
			err = encoder.Encode(sites)
		}
	} else {
		var matrix capability.Matrix
		matrix, err = capability.Load()
		if err == nil {
			err = os.WriteFile(filepath.Join(root, "docs/mcp-capability-matrix.md"), []byte(capability.RenderMarkdown(matrix)), 0644)
		}
	}
	return err
}
