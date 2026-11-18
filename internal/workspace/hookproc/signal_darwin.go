package hookproc

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
)

type auditToken [8]uint32

type hookProcessInstances struct {
	capture func(int) (auditToken, error)
	tokens  map[int]auditToken
}

func platformHookSignals(lifetime *Lifetime) hookSignals {
	instances := new(hookProcessInstances)
	if lifetime != nil {
		instances = &lifetime.instances
	}
	if instances.capture == nil {
		instances.capture = captureHookToken
	}
	return instances
}

type machProcessAPI struct {
	taskSelf       func() uint32
	taskNameForPID func(uint32, int32, *uint32) int32
	taskInfo       func(uint32, int32, *auditToken, *uint32) int32
	portDeallocate func(uint32, uint32) int32
	signal         func(*auditToken, int32) int32
}

var loadMachProcessAPI = sync.OnceValues(func() (*machProcessAPI, error) {
	api := new(machProcessAPI)
	for _, symbol := range []struct {
		name string
		fn   any
	}{
		{"mach_task_self", &api.taskSelf},
		{"task_name_for_pid", &api.taskNameForPID},
		{"task_info", &api.taskInfo},
		{"mach_port_deallocate", &api.portDeallocate},
		{"proc_signal_with_audittoken", &api.signal},
	} {
		address, err := purego.Dlsym(purego.RTLD_DEFAULT, symbol.name)
		if err != nil {
			return nil, fmt.Errorf("resolve %s: %w", symbol.name, err)
		}
		purego.RegisterFunc(symbol.fn, address)
	}
	return api, nil
})

func captureHookToken(pid int) (auditToken, error) {
	var token auditToken
	api, err := loadMachProcessAPI()
	if err != nil {
		return token, err
	}
	self := api.taskSelf()
	var task uint32
	if code := api.taskNameForPID(self, int32(pid), &task); code != 0 {
		return token, fmt.Errorf("obtain hook task name for PID %d: Mach error %d", pid, code)
	}
	const taskAuditToken = 15
	count := uint32(len(token))
	code := api.taskInfo(task, taskAuditToken, &token, &count)
	release := api.portDeallocate(self, task)
	if code != 0 || count != uint32(len(token)) {
		err = fmt.Errorf("read hook audit token for PID %d: Mach result %d, count %d", pid, code, count)
	}
	if release != 0 {
		err = errors.Join(err, fmt.Errorf("release hook task name: Mach error %d", release))
	}
	return token, err
}

func (instances *hookProcessInstances) prepare(pid int) (func(), error) {
	if _, exists := instances.tokens[pid]; exists {
		return nil, nil
	}
	token, err := instances.capture(pid)
	if err != nil {
		return nil, err
	}
	return func() {
		if instances.tokens == nil {
			instances.tokens = make(map[int]auditToken)
		}
		instances.tokens[pid] = token
	}, nil
}

func (instances *hookProcessInstances) signal(pid int, sig syscall.Signal) error {
	token, ok := instances.tokens[pid]
	if !ok {
		return fmt.Errorf("missing verified hook audit token for PID %d", pid)
	}
	api, err := loadMachProcessAPI()
	if err != nil {
		return err
	}
	if code := api.signal(&token, int32(sig)); code != 0 {
		return syscall.Errno(code)
	}
	return nil
}
