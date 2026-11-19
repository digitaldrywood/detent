//go:build unix && !darwin && !linux

package procgroup

func inspectProcess(pid int) (Identity, error) { return inspectOtherUnixProcess(pid) }

func inspectProcessGroup(group int) ([]processGroupMember, error) {
	return inspectOtherUnixGroup(group)
}
