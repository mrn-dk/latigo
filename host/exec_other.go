//go:build !linux

package host

import (
	"fmt"
	"os/exec"
)

// networkIsolate cannot guarantee network isolation off Linux, so it fails
// closed when networking is disabled: the operator must explicitly set
// ExecPolicy.Network to run native commands here, accepting that they are not
// network-isolated.
func networkIsolate(_ *exec.Cmd, allowNetwork bool) error {
	if allowNetwork {
		return nil
	}
	return fmt.Errorf("network isolation is unavailable on this platform; set ExecPolicy.Network to run without it")
}
