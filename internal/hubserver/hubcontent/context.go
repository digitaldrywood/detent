package hubcontent

import (
	"sync"

	"github.com/digitaldrywood/detent/internal/attachment"
	"github.com/digitaldrywood/detent/internal/tracker"
)

type RunDetailContextKey struct{}

type RunDetailContext struct {
	Mu           sync.Mutex                                  `json:"-"`
	Store        attachment.DetailStore                      `json:"-"`
	Organization string                                      `json:"-"`
	Reference    *attachment.ArchiveReference                `json:"-"`
	Runs         map[string]tracker.NativeRunData            `json:"-"`
	Cache        map[string]map[string]tracker.NativeRunData `json:"-"`
}
