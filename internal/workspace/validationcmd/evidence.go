package validationcmd

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/digitaldrywood/detent/internal/gate"
	"github.com/digitaldrywood/detent/internal/gobudget"
	commandshell "github.com/digitaldrywood/detent/internal/shell"
	"github.com/digitaldrywood/detent/internal/workspace/gitexec"
)

type Inputs struct {
	Dir  string
	Path string
	Args []string
	Env  []string
}

func (command Inputs) Evidence(ctx context.Context, validation string) *gate.CommandEvidence {
	if validation == "go version" {
		return command.Environment(ctx)
	}
	return &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
}

func (command Inputs) probe(ctx context.Context, text string) ([]byte, error) {
	if command.Path == "" || len(command.Args) < 2 {
		return nil, errors.New("missing validation shell")
	}
	probe := commandshell.Command(ctx, text, command.Path)
	if len(probe.Args) < 2 || !slices.Equal(probe.Args[1:len(probe.Args)-1], command.Args[1:len(command.Args)-1]) {
		return nil, errors.New("validation shell arguments differ")
	}
	probe.Dir, probe.Env, probe.WaitDelay = command.Dir, slices.Clone(command.Env), gitexec.WaitDelay
	return commandshell.CommandOutput(ctx, probe, false)
}

func (command Inputs) Environment(ctx context.Context) *gate.CommandEvidence {
	evidence := &gate.CommandEvidence{Checks: []gate.CheckObservation{}}
	var observed struct {
		OS           string `json:"GOHOSTOS"`
		Architecture string `json:"GOHOSTARCH"`
		Version      string `json:"GOVERSION"`
	}
	output, err := command.probe(ctx, "go env -json")
	if err == nil && json.Unmarshal(output, &observed) == nil {
		environment := gate.CheckEnvironment{OS: observed.OS, Architecture: observed.Architecture, GoVersion: observed.Version}
		if environment.Valid() {
			evidence.Environment = environment
		}
	}
	return evidence
}

func GoBudget(ctx context.Context, environment []string) (map[string]string, error) {
	inherited := make(map[string]string)
	for _, entry := range environment {
		key, value, ok := strings.Cut(entry, "=")
		if ok {
			inherited[key] = value
		}
	}
	variables, err := gobudget.FromContext(ctx).Environment(inherited["GOFLAGS"])
	if err != nil {
		return nil, err
	}
	for _, key := range []string{"GOMAXPROCS", gobudget.TestProcsEnvironment} {
		if _, explicit := inherited[key]; explicit {
			delete(variables, key)
		}
	}
	return variables, nil
}
