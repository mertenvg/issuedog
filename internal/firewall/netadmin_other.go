//go:build !linux

package firewall

import (
	"errors"
	"syscall"
)

func netAdmin() (*syscall.SysProcAttr, error) {
	return nil, errors.New("the agent firewall needs Linux")
}
