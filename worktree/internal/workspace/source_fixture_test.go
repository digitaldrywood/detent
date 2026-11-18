package workspace

import (
	"testing"

	"github.com/digitaldrywood/detent/internal/workspace/internal/gitfixture"
)

func runWorkspaceTests(m *testing.M) int         { stubReapProcessScanner(); return gitfixture.RunTests(m) }
func initSourceRepo(t *testing.T) string         { t.Helper(); return gitfixture.SourceRepo(t) }
func initSourceRepoAt(t *testing.T, path string) { t.Helper(); gitfixture.SourceRepoAt(t, path) }
func initBareRemote(t *testing.T) string         { t.Helper(); return gitfixture.BareRemote(t) }
func runGit(t *testing.T, path string, args ...string) string {
	t.Helper()
	return gitfixture.Git(t, path, args...)
}
func runCommand(t *testing.T, path, name string, args ...string) string {
	t.Helper()
	return gitfixture.Command(t, path, name, args...)
}
func readFile(t *testing.T, path string) string { t.Helper(); return gitfixture.ReadFile(t, path) }
func checkpointWrite(t *testing.T, root, path, content string) {
	t.Helper()
	gitfixture.WriteFile(t, root, path, content)
}
