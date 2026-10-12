package firewall

import "syscall"

// capNetAdmin is CAP_NET_ADMIN from linux/capability.h. golang.org/x/sys
// isn't a dependency, so it's defined here.
const capNetAdmin = 12

func netAdmin() (*syscall.SysProcAttr, error) {
	return &syscall.SysProcAttr{AmbientCaps: []uintptr{capNetAdmin}}, nil
}
