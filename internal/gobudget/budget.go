package gobudget

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	WrapperName          = "detent-go-budget"
	DirEnvironment       = "DETENT_GO_BUDGET_DIR"
	SlotsEnvironment     = "DETENT_GO_BUDGET_SLOTS"
	invocationsPerBudget = 4
)

type Budget struct {
	Slots      int
	Dir        string
	Executable string
}

func New(slots int, dir string, executable string) Budget {
	if slots <= 0 {
		slots = runtime.NumCPU()
	}
	return Budget{Slots: slots, Dir: strings.TrimSpace(dir), Executable: strings.TrimSpace(executable)}
}

func HostDir() string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d", WrapperName, os.Getuid()))
}

func (b Budget) Enabled() bool {
	return b.Slots > 0 && b.Dir != "" && b.Executable != ""
}

func (b Budget) Width() int {
	return max(1, b.Slots/invocationsPerBudget)
}

func (b Budget) WrapperPath() string {
	return filepath.Join(b.Dir, "bin", WrapperName)
}

func (b Budget) Environment(inheritedGOFLAGS string) (map[string]string, error) {
	if !b.Enabled() {
		return map[string]string{}, nil
	}
	if err := b.prepare(); err != nil {
		return nil, err
	}
	width := strconv.Itoa(b.Width())
	return map[string]string{
		"GOFLAGS":        goflags(inheritedGOFLAGS, width, b.WrapperPath()),
		"GOMAXPROCS":     width,
		DirEnvironment:   b.Dir,
		SlotsEnvironment: strconv.Itoa(b.Slots),
	}, nil
}

func (b Budget) prepare() error {
	if err := os.MkdirAll(filepath.Join(b.Dir, "bin"), 0o755); err != nil {
		return fmt.Errorf("create go build budget directory: %w", err)
	}
	for slot := range b.Slots {
		file, err := os.OpenFile(slotPath(b.Dir, slot), os.O_RDONLY|os.O_CREATE, 0o600)
		if err != nil {
			return fmt.Errorf("create go build budget slot: %w", err)
		}
		if err := file.Close(); err != nil {
			return fmt.Errorf("close go build budget slot: %w", err)
		}
	}
	return linkWrapper(b.WrapperPath(), b.Executable)
}

func linkWrapper(path string, executable string) error {
	if current, err := os.Readlink(path); err == nil && current == executable {
		return nil
	}
	staged := fmt.Sprintf("%s.%d", path, os.Getpid())
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove staged go build budget wrapper: %w", err)
	}
	if err := os.Symlink(executable, staged); err != nil {
		return fmt.Errorf("link go build budget wrapper: %w", err)
	}
	if err := os.Rename(staged, path); err != nil {
		return errors.Join(fmt.Errorf("install go build budget wrapper: %w", err), os.Remove(staged))
	}
	return nil
}

func slotPath(dir string, slot int) string {
	return filepath.Join(dir, fmt.Sprintf("slot-%03d", slot))
}

func goflags(inherited string, width string, wrapper string) string {
	flags := strings.Fields(inherited)
	if !hasFlag(flags, "p") {
		flags = append(flags, "-p="+width)
	}
	if !hasFlag(flags, "toolexec") && !strings.ContainsAny(wrapper, " \t\r\n'\"") {
		flags = append(flags, "-toolexec="+wrapper)
	}
	return strings.Join(flags, " ")
}

func hasFlag(flags []string, name string) bool {
	for _, flag := range flags {
		flag = strings.TrimPrefix(strings.TrimPrefix(flag, "-"), "-")
		if flag == name || strings.HasPrefix(flag, name+"=") {
			return true
		}
	}
	return false
}
