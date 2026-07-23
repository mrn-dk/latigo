package host

import (
	"os"
	"os/exec"
	"syscall"
)

// networkIsolate places the child in a fresh, empty network namespace when
// networking is disabled, so it cannot reach anything off-host. It pairs a user
// namespace with the network namespace so this works for an unprivileged host
// process (the standard unprivileged-isolation technique). If the kernel refuses
// (e.g. unprivileged user namespaces are disabled), Start will fail and the
// caller fails closed rather than silently running with network access.
func networkIsolate(cmd *exec.Cmd, allowNetwork bool) error {
	if allowNetwork {
		return nil
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWUSER | syscall.CLONE_NEWNET,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
	}
	return nil
}
