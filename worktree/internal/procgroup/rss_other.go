//go:build !unix

package procgroup

import "context"

func processGroupRSS(context.Context, Identity) (uint64, error) {
	return 0, ErrRSSUnsupported
}
