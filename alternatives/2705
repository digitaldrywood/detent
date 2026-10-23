package project

import "github.com/digitaldrywood/detent/internal/toolcache"

// HostCache returns the host measurement recorded by the existing reaper.
func (p *Project) HostCache() *toolcache.Report {
	if p.hostCacheReport == nil {
		return nil
	}
	return p.hostCacheReport()
}
