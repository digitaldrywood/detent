package claudecode

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os/exec"
	"reflect"
	"strings"
	"time"

	"github.com/digitaldrywood/detent/internal/isolation"
	"github.com/digitaldrywood/detent/internal/procgroup"
)

func IsolationSettings(p isolation.Policy) (map[string]any, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	sandbox := map[string]any{"enabled": false}
	settings := map[string]any{"sandbox": sandbox}
	if p.Tier == isolation.NativeTrusted {
		return settings, nil
	}
	sockets := []string{}
	for _, service := range p.HostServices {
		sockets = append(sockets, strings.TrimPrefix(service, "unix:"))
	}
	sandbox["enabled"] = true
	sandbox["failIfUnavailable"] = true
	sandbox["allowUnsandboxedCommands"] = false
	sandbox["autoAllowBashIfSandboxed"] = true
	sandbox["excludedCommands"] = []string{}
	sandbox["enableWeakerNestedSandbox"] = false
	sandbox["allowAppleEvents"] = false
	sandbox["filesystem"] = map[string]any{"allowWrite": p.WritableRoots, "denyRead": []string{"~/.ssh", "~/.aws", "~/Library/Keychains", "/Library/Keychains"}, "disabled": false}
	domains := isolation.Domains()
	domains = append(domains, p.ExtraNetworkDomains...)
	if p.AllowLocalBinding {
		domains = append(domains, "localhost", "127.0.0.1", "::1")
	}
	sandbox["network"] = map[string]any{"allowedDomains": domains, "allowUnixSockets": sockets, "allowAllUnixSockets": false, "allowLocalBinding": p.AllowLocalBinding, "strictAllowlist": true}
	settings["permissions"] = map[string]any{"disableBypassPermissionsMode": "disable", "deny": []string{"Read(~/.ssh/**)", "Read(~/.aws/**)", "Read(~/Library/Keychains/**)", "Read(//Library/Keychains/**)"}}
	return settings, nil
}

func VerifySandboxCommand(ctx context.Context, cmd *exec.Cmd, settings map[string]any) error {
	procgroup.Configure(ctx, cmd)
	cmd.Cancel = func() error { return procgroup.TerminateTree(cmd, procgroup.GroupID(cmd)) }
	cmd.WaitDelay = time.Second
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		return errors.Join(err, input.Close())
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return errors.Join(err, input.Close())
	}
	groupID := procgroup.GroupID(cmd)
	_, err = verifySandboxProcess(ctx, input, output, settings)
	err = errors.Join(err, input.Close())
	if err != nil {
		err = errors.Join(err, procgroup.TerminateTree(cmd, groupID))
	}
	waitErr := cmd.Wait()
	return errors.Join(err, waitErr, procgroup.Cleanup(groupID))
}

func verifySandboxProcess(ctx context.Context, input io.Writer, output io.Reader, settings map[string]any) (*bufio.Reader, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	reader := bufio.NewReaderSize(output, 1<<20)
	done := make(chan error, 1)
	go func() {
		encoder := json.NewEncoder(input)
		for _, subtype := range []string{"initialize", "get_settings"} {
			if err := encoder.Encode(map[string]any{"type": "control_request", "request_id": subtype, "request": map[string]any{"subtype": subtype}}); err != nil {
				done <- err
				return
			}
			for {
				line, err := reader.ReadSlice('\n')
				if err != nil {
					done <- err
					return
				}
				var response struct {
					Type     string `json:"type"`
					Response struct {
						Subtype   string `json:"subtype"`
						RequestID string `json:"request_id"`
						Response  struct {
							Effective map[string]any `json:"effective"`
						} `json:"response"`
					} `json:"response"`
				}
				if err := json.Unmarshal(line, &response); err != nil {
					done <- errors.New("invalid Claude isolation response")
					return
				}
				if response.Type != "control_response" || response.Response.RequestID != subtype {
					continue
				}
				if response.Response.Subtype != "success" {
					done <- errors.New("claude cannot verify effective isolation settings")
					return
				}
				if subtype == "get_settings" {
					if err := VerifyEffectiveIsolation(settings, response.Response.Response.Effective); err != nil {
						done <- err
						return
					}
				}
				break
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return reader, err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func VerifyEffectiveIsolation(expected, effective map[string]any) error {
	encoded, err := json.Marshal(expected)
	if err != nil {
		return err
	}
	var normalized map[string]any
	if err := json.Unmarshal(encoded, &normalized); err != nil {
		return err
	}
	if !containsIsolationSettings(effective, normalized) {
		return errors.New("claude effective settings do not enforce runner isolation")
	}
	return nil
}

func containsIsolationSettings(actual, expected map[string]any) bool {
	for key, value := range expected {
		got, exists := actual[key]
		if !exists {
			return false
		}
		if nested, ok := value.(map[string]any); ok {
			provided, ok := got.(map[string]any)
			if !ok || !containsIsolationSettings(provided, nested) {
				return false
			}
		} else if !reflect.DeepEqual(got, value) {
			return false
		}
	}
	return true
}
