// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

//go:build !windows

package main

import (
	"os"
	"syscall"
)

// lockFolder takes the kernel lock on this folder's lock file and holds it for
// the life of the process. The kernel drops it on ANY exit, including a kill, so
// it can never be stale. ok is false when another process holds it.
func lockFolder(path string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

// pidIsHelper reports whether pid is a live process that looks like this
// program. It exists for a helper started by an older build, which holds no lock.
func pidIsHelper(pid int) bool {
	if pid <= 0 || pid == os.Getpid() {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return false
	}
	return processNameContains(pid, "owlka-whatsapp")
}
