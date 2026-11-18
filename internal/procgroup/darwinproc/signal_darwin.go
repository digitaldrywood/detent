package darwinproc

import (
	"errors"
	"fmt"
	"sync"
	"syscall"

	"github.com/ebitengine/purego"
)

type AuditToken [8]uint32

type machProcessAPI struct {
	taskSelf       func() uint32
	taskNameForPID func(uint32, int32, *uint32) int32
	taskInfo       func(uint32, int32, *AuditToken, *uint32) int32
	portDeallocate func(uint32, uint32) int32
	signal         func(*AuditToken, int32) int32
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

func Capture(pid int) (AuditToken, error) {
	var token AuditToken
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

func (token AuditToken) Signal(sig syscall.Signal) error {
	api, err := loadMachProcessAPI()
	if err != nil {
		return err
	}
	if code := api.signal(&token, int32(sig)); code != 0 {
		return syscall.Errno(code)
	}
	return nil
}
