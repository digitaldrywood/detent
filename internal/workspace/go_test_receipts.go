package workspace

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/gotestreceipt"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

func (l *LocalGit) startGoTestReceipts(ctx context.Context, cmd *exec.Cmd, root, scratch string) *gotestreceipt.Session {
	config, ok := gotestreceipt.FromContext(ctx)
	if !ok {
		return nil
	}
	session, err := gotestreceipt.Start(ctx, config.Store, l.logger, scratch)
	if err != nil {
		l.logger.Warn("go test receipts unavailable; validation tests execute", "error", err)
		return nil
	}
	goflags := ""
	for _, entry := range cmd.Environ() {
		if value, ok := strings.CutPrefix(entry, "GOFLAGS="); ok {
			goflags = value
		}
	}
	variables := session.Environment()
	variables[gotestreceipt.RootEnvironment] = root
	variables["GOFLAGS"] = gotestreceipt.Goflags(goflags, config.Wrapper)
	procgroup.SetEnvironment(cmd, procgroup.Environment{Variables: variables})
	return session
}

func (l *LocalGit) finishGoTestReceipts(ctx context.Context, session *gotestreceipt.Session, result *gate.CommandResult) {
	stats, err := session.Close(ctx)
	if err != nil {
		l.logger.Warn("close go test receipt session", "error", err)
	}
	if stats.Lookups == 0 {
		return
	}
	l.logger.Info("go test receipts", "lookups", stats.Lookups, "reused", stats.Hits, "recorded", stats.Records, "refused", stats.Refused, "failures", stats.Failures)
	result.Output += fmt.Sprintf("\ngo test receipts: %d of %d test binaries reused, %d recorded, %d not recordable, %d receipt service failures\n", stats.Hits, stats.Lookups, stats.Records, stats.Refused, stats.Failures)
}
