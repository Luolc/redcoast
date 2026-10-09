//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// ioctlReadTermios is the ioctl that succeeds only on a terminal.
const ioctlReadTermios = unix.TCGETS

// adoptOrphans makes this process the parent of any descendant whose own parent
// exits (a child subreaper), so that what claude leaves behind can be found and
// reaped instead of going to init.
func adoptOrphans() error {
	return unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0)
}

// process is one live descendant: its PID and process group.
type process struct {
	pid, group int
}

// processTable reads every process's parent and process group from /proc.
func processTable() (parent, group map[int]int) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, nil
	}
	parent = make(map[int]int)
	group = make(map[int]int)
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		stat, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "stat"))
		if err != nil {
			continue
		}
		// Fields after the parenthesized command name: state, ppid, pgrp, ... The name
		// itself may contain ")" and spaces, so the split is at the last ")".
		closing := strings.LastIndexByte(string(stat), ')')
		if closing < 0 {
			continue
		}
		fields := strings.Fields(string(stat)[closing+1:])
		if len(fields) < 3 {
			continue
		}
		ppid, errP := strconv.Atoi(fields[1])
		pgrp, errG := strconv.Atoi(fields[2])
		if errP != nil || errG != nil {
			continue
		}
		parent[pid] = ppid
		group[pid] = pgrp
	}
	return parent, group
}

// descendants lists every process below this one, read from /proc.
func descendants() []process {
	parent, group := processTable()
	self := os.Getpid()
	var found []process
	for pid, ppid := range parent {
		for ancestor := ppid; ancestor > 1; ancestor = parent[ancestor] {
			if ancestor == self {
				found = append(found, process{pid, group[pid]})
				break
			}
		}
	}
	return found
}

// reapChildren collects exited children so they do not linger as zombies.
func reapChildren() {
	for {
		var status unix.WaitStatus
		pid, err := unix.Wait4(-1, &status, unix.WNOHANG, nil)
		if pid <= 0 || err != nil {
			return
		}
	}
}

// reapOrphans collects exited children other than claude while claude runs. Only
// adopted orphans are waited for, each by its own PID: claude's exit status belongs to
// cmd.Wait, which a wait for any child could take away.
func reapOrphans(claude int) {
	parent, _ := processTable()
	self := os.Getpid()
	for pid, ppid := range parent {
		if ppid == self && pid != claude {
			var status unix.WaitStatus
			_, _ = unix.Wait4(pid, &status, unix.WNOHANG, nil)
		}
	}
}
