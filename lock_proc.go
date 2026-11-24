// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

//go:build !windows

package main

import (
	"os/exec"
	"strconv"
	"strings"
)

func processNameContains(pid int, want string) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output()
	return err == nil && strings.Contains(string(out), want)
}
