package workspacehub

import "github.com/digitaldrywood/detent/internal/workspacesession"

func RelayedRequires(requires []string) []string {
	relayed := make([]string, 0, len(requires))
	for _, name := range requires {
		switch name {
		case workspacesession.CapabilityDiff, workspacesession.CapabilityPreview:
			continue
		}
		relayed = append(relayed, name)
	}
	return relayed
}
