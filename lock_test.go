// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/types/events"
)

func TestFolderLockAllowsOneHolderAndFreesOnClose(t *testing.T) {
	p := filepath.Join(t.TempDir(), lockName)
	f1, ok, err := lockFolder(p)
	if err != nil || !ok {
		t.Fatalf("first lock: ok=%v err=%v", ok, err)
	}
	if f2, ok, err := lockFolder(p); err != nil || ok {
		if f2 != nil {
			f2.Close()
		}
		t.Fatalf("second lock must be refused while held: ok=%v err=%v", ok, err)
	}
	f1.Close() // the kernel does this on any exit, including a kill
	f3, ok, err := lockFolder(p)
	if err != nil || !ok {
		t.Fatalf("lock after release: ok=%v err=%v", ok, err)
	}
	f3.Close()
}

// The 24 Nov 2026 incident: a helper that is alive but too busy to answer. A
// second start on its folder must exit quietly and touch nothing, whatever
// helper.json says about a port that is not answering.
func TestSecondStartOnLockedFolderTouchesNothing(t *testing.T) {
	dir := t.TempDir()
	held, ok, err := lockFolder(filepath.Join(dir, lockName))
	if err != nil || !ok {
		t.Fatal("setup lock", err)
	}
	defer held.Close()
	stale := []byte(`{"port":1,"token":"x","pid":1,"version":"0.1.0"}`)
	if err := os.WriteFile(filepath.Join(dir, stateName), stale, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- run(dir, true) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a second start on a locked folder must exit 0, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second start did not exit: it went on to run a second helper on a locked folder")
	}
	got, _ := os.ReadFile(filepath.Join(dir, stateName))
	if string(got) != string(stale) {
		t.Fatalf("helper.json was rewritten by the second start: %s", got)
	}
	for _, n := range []string{"messages.db", "session.db", "helper.log"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("%s was created by the second start", n)
		}
	}
}

func TestPidIsHelperSeesAnOlderBuildWithoutALock(t *testing.T) {
	if pidIsHelper(os.Getpid()) || pidIsHelper(0) || pidIsHelper(-5) {
		t.Fatal("this process, and non-pids, are never 'another helper'")
	}
	fake := filepath.Join(t.TempDir(), "owlka-whatsapp")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(fake)
	if err := cmd.Start(); err != nil {
		t.Skip("cannot start the stand-in:", err)
	}
	time.Sleep(200 * time.Millisecond) // let sh exec the script
	pid := cmd.Process.Pid
	if !pidIsHelper(pid) {
		t.Fatal("a live process named owlka-whatsapp must count as a running helper")
	}
	_ = cmd.Process.Kill()
	_, _ = cmd.Process.Wait()
	if pidIsHelper(pid) {
		t.Fatal("a dead pid must not count")
	}
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func instantMgr(r *rig) {
	r.m.sleep = func(context.Context, time.Duration) {}
	r.m.takeoverDelays = []time.Duration{time.Second, time.Second, time.Second}
}

func TestReconnectsByItselfAfterAnotherCopyTookOver(t *testing.T) {
	r := newRig(t)
	instantMgr(r)
	before := r.client.connects
	r.client.mu.Lock()
	r.client.connected = false
	r.client.mu.Unlock()
	r.client.emit(&events.StreamReplaced{})
	eventually(t, "connected again", func() bool { return r.m.Accounts()[0].Usable })
	r.client.mu.Lock()
	n := r.client.connects
	r.client.mu.Unlock()
	if n != before+1 {
		t.Fatalf("expected exactly one reconnect, connects %d -> %d", before, n)
	}
}

func TestSaysSoWhenItCannotReconnect(t *testing.T) {
	r := newRig(t)
	instantMgr(r)
	r.client.mu.Lock()
	r.client.connected = false
	r.client.connectErr = os.ErrDeadlineExceeded
	r.client.mu.Unlock()
	r.client.emit(&events.StreamReplaced{})
	eventually(t, "gives up and says so", func() bool {
		i := r.m.Accounts()[0]
		return !i.Usable && i.State == "connecting" && i.Detail != "" && contains(i.Detail, "could not reconnect")
	})
}

func TestStopsFightingAnotherUserOfTheSession(t *testing.T) {
	r := newRig(t)
	instantMgr(r)
	for i := 0; i <= maxTakeoversPerHour; i++ {
		r.client.emit(&events.StreamReplaced{})
		eventually(t, "settled", func() bool {
			a := r.m.accounts[me]
			a.mu.Lock()
			defer a.mu.Unlock()
			return len(a.takeovers) == i+1 && !a.recovering
		})
	}
	eventually(t, "says it stopped", func() bool { return contains(r.m.Accounts()[0].Detail, "stopped reconnecting") })
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
