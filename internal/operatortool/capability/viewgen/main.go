// viewgen renders the review document or lists independently discovered source
// sites for fixture authors. It is not a Detent CLI subcommand or runtime gate.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/digitaldrywood/detent/internal/operatortool/capability"
	"os"
	"path/filepath"
)

func main() {
	root := flag.String("root", "../../..", "repository root (default: from the capability package)")
	sources := flag.Bool("sources", false, "print discovered source sites instead of rendering documentation")
	flag.Parse()
	var err error
	if *sources {
		var sites []capability.Candidate
		sites, err = capability.Discover(os.DirFS(*root))
		if err == nil {
			encoder := json.NewEncoder(os.Stdout)
			encoder.SetIndent("", "  ")
			err = encoder.Encode(sites)
		}
	} else {
		var matrix capability.Matrix
		matrix, err = capability.Load()
		if err == nil {
			err = os.WriteFile(filepath.Join(*root, "docs/mcp-capability-matrix.md"), []byte(capability.RenderMarkdown(matrix)), 0644)
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
