//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package agent

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TIOCGETA
