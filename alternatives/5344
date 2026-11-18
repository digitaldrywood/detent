package workspace

import (
	"github.com/digitaldrywood/detent/internal/workspace/internal/workpath"
)

const workerScratchBaseName = workpath.WorkerScratchBaseName

func WorkerScratchRoot(path string) string              { return workpath.WorkerScratchRoot(path) }
func workerScratchRoots(path string) []string           { return workpath.WorkerScratchRoots(path) }
func workerScratchGroup(path string) string             { return workpath.WorkerScratchGroup(path) }
func workerScratchGroupIn(base, path string) string     { return workpath.WorkerScratchGroupIn(base, path) }
func workerScratchBase() string                         { return workpath.WorkerScratchBase() }
func sharedWorkerScratchBase() string                   { return workpath.SharedWorkerScratchBase() }
func bestEffortCanonicalPath(path string) string        { return workpath.BestEffortCanonicalPath(path) }
func canonicalExistingPath(path string) (string, error) { return workpath.CanonicalExistingPath(path) }
func expandPath(path string) (string, error)            { return workpath.ExpandPath(path) }
func pathWithin(root, path string) bool                 { return workpath.PathWithin(root, path) }
