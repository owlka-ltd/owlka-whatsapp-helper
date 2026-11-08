// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// sendBodies is every way a caller can ask /send to write to WhatsApp.
func sendBodies(t *testing.T) map[string]map[string]any {
	file := filepath.Join(t.TempDir(), "pic.png")
	if err := os.WriteFile(file, []byte("PNG"), 0o600); err != nil {
		t.Fatal(err)
	}
	return map[string]map[string]any{
		"text":           {"to": alice, "text": "hello"},
		"reply":          {"to": alice, "text": "hello", "reply_to": "IN1"},
		"file":           {"to": alice, "file": file, "text": "caption"},
		"new number":     {"to": "447700900222", "text": "hi", "confirm_new_number": "447700900222"},
		"approved":       {"to": alice, "text": "hello", "approved": true},
		"unknown fields": {"to": alice, "text": "hello", "approved": true, "confirm_new_number": "447700900222"},
	}
}

func TestReadOnly_EverySendPathIsRefusedAndNeverReachesTheClient(t *testing.T) {
	r := newRigMode(t, true)
	r.trace = nil
	for name, body := range sendBodies(t) {
		body["account"] = me
		status, out := r.call("POST", "/send", body)
		e, _ := out["error"].(map[string]any)
		if status != 403 || e == nil || e["code"] != "read_only" || e["message"] == "" {
			t.Errorf("send (%s): %d %v", name, status, out)
		}
	}
	status, out := r.call("POST", "/read", map[string]any{"account": me, "chat": alice})
	if e, _ := out["error"].(map[string]any); status != 403 || e == nil || e["code"] != "read_only" {
		t.Errorf("read: %d %v", status, out)
	}
	if len(r.client.sent) != 0 || len(r.client.read) != 0 || len(r.trace) != 0 || len(r.sleeps) != 0 {
		t.Fatalf("the client was reached: sent=%v read=%v trace=%v sleeps=%v", r.client.sent, r.client.read, r.trace, r.sleeps)
	}
	// The unread count is untouched: nothing was marked read locally either.
	_, chats := r.call("GET", "/chats?account="+me+"&unread=1", nil)
	if len(chats["chats"].([]any)) != 1 {
		t.Fatal("a refused mark-read still cleared the unread count")
	}
}

func TestReadOnly_TheWrappedClientRefusesWritesEvenIfTheManagerGuardIsBypassed(t *testing.T) {
	r := newRigMode(t, true)
	r.trace = nil
	a, ref := r.m.account(me)
	if ref != nil {
		t.Fatal(ref)
	}
	ctx := context.Background()
	to, _ := r.m.ResolveChat(a, alice)
	_ = to
	jid := a.client.JID()
	if _, _, err := a.client.Send(ctx, jid, nil); err == nil {
		t.Error("Send reached the client")
	}
	if err := a.client.MarkRead(ctx, []string{"IN1"}, r.clock, jid, jid); err == nil {
		t.Error("MarkRead reached the client")
	}
	if _, err := a.client.Upload(ctx, []byte("x"), ""); err == nil {
		t.Error("Upload reached the client")
	}
	if err := a.client.SetTyping(ctx, jid, true); err != nil {
		t.Errorf("typing should be a quiet no-op, got %v", err)
	}
	_ = a.client.SetTyping(ctx, jid, false)
	if len(r.client.sent) != 0 || len(r.client.read) != 0 || len(r.trace) != 0 {
		t.Fatalf("fake was touched: sent=%v read=%v trace=%v", r.client.sent, r.client.read, r.trace)
	}
}

func TestReadOnly_LinkingReadingAndListingStillWork(t *testing.T) {
	r := newRigMode(t, true)
	if status, out := r.call("GET", "/messages?account="+me+"&chat="+alice, nil); status != 200 || len(out["messages"].([]any)) != 1 {
		t.Fatalf("messages: %d %v", status, out)
	}
	if status, _ := r.call("GET", "/chats?account="+me, nil); status != 200 {
		t.Fatal("chats")
	}
	if status, _ := r.call("GET", "/search?account="+me+"&query=Friday", nil); status != 200 {
		t.Fatal("search")
	}
	if status, out := r.call("POST", "/link/start", map[string]any{"phone": "+44 7700 900333"}); status != 200 || out["code"] == "" {
		t.Fatalf("linking: %d %v", status, out)
	}
	_, health := r.call("GET", "/health", nil)
	if health["read_only"] != true {
		t.Fatalf("health does not say read-only: %v", health)
	}
}

func TestDefaultMode_SendReadAndTypingStillReachTheClient(t *testing.T) {
	r := newRigMode(t, false)
	r.trace = nil
	for name, body := range sendBodies(t) {
		if name != "text" {
			continue
		}
		if status, msg := r.send(body); status != 200 {
			t.Fatalf("send: %d %s", status, msg)
		}
	}
	if len(r.client.sent) != 1 {
		t.Fatalf("send did not reach the client: %v", r.client.sent)
	}
	typed := false
	for _, s := range r.trace {
		typed = typed || s == "typing=true"
	}
	if !typed {
		t.Fatalf("typing did not reach the client: %v", r.trace)
	}
	if status, _ := r.call("POST", "/read", map[string]any{"account": me, "chat": alice}); status != 200 || len(r.client.read) != 1 {
		t.Fatalf("mark read did not reach the client: %d %v", status, r.client.read)
	}
	_, health := r.call("GET", "/health", nil)
	if health["read_only"] != false {
		t.Fatalf("health: %v", health)
	}
}
