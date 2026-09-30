//go:build aix || linux || solaris || zos

package agent

import "golang.org/x/sys/unix"

const ioctlReadTermios = unix.TCGETS
