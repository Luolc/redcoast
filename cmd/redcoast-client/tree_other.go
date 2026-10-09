//go:build !linux

package main

import "golang.org/x/sys/unix"

// ioctlReadTermios is the ioctl that succeeds only on a terminal.
const ioctlReadTermios = unix.TIOCGETA

// adoptOrphans is Linux-only; elsewhere only claude's process group is reaped.
func adoptOrphans() error { return nil }

// process is one live descendant: its PID and process group.
type process struct {
	pid, group int
}

// descendants is Linux-only; elsewhere the tree beyond claude's group is not visible.
func descendants() []process { return nil }

// reapChildren is Linux-only.
func reapChildren() {}

// reapOrphans is Linux-only: elsewhere no orphans are adopted.
func reapOrphans(int) {}
