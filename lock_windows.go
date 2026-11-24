// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// lockFolder: see lock_unix.go. LockFileEx is released by Windows when the
// process ends, however it ends.
func lockFolder(path string) (f *os.File, ok bool, err error) {
	f, err = os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, false, err
	}
	ol := new(windows.Overlapped)
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if err != nil {
		f.Close()
		if err == windows.ERROR_LOCK_VIOLATION {
			return nil, false, nil
		}
		return nil, false, err
	}
	return f, true, nil
}

// pidIsHelper: older builds hold no lock, but the Windows build never shipped
// without it, so there is nothing to check.
func pidIsHelper(int) bool { return false }
