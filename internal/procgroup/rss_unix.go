//go:build unix

package procgroup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

func processGroupRSS(ctx context.Context, identity Identity) (uint64, error) {
	alive, err := Alive(identity)
	if err != nil {
		return 0, err
	}
	if !alive {
		return 0, ErrProcessNotRunning
	}
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pgid=,rss=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	output, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("inspect process group RSS: %w", err)
	}
	return parseProcessGroupRSS(string(output), identity.GroupID)
}

func parseProcessGroupRSS(output string, groupID int) (uint64, error) {
	if groupID <= 0 {
		return 0, ErrProcessNotRunning
	}
	var totalKiB uint64
	for line := range strings.Lines(output) {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != strconv.Itoa(groupID) {
			continue
		}
		rssKiB, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, fmt.Errorf("parse process group %d RSS %q: %w", groupID, fields[1], err)
		}
		totalKiB += rssKiB
	}
	return totalKiB * 1024, nil
}
