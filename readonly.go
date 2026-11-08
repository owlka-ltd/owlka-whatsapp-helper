// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

package main

import (
	"context"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// ReadOnly mode (--read-only): this helper may link, read history and list
// chats, and must never write anything to WhatsApp. Two guards:
//  1. Manager.Send and Manager.MarkRead refuse with code "read_only" before
//     doing any work.
//  2. The Client the manager holds is wrapped, so even a new code path that
//     forgets guard 1 cannot reach the real send, mark-read, upload or
//     presence calls. SetTyping becomes a no-op.
//
// whatsmeow itself never sends read receipts (only an explicit MarkRead
// does) and never sends presence unless asked, so nothing else is needed for
// those. It does send the protocol-level delivery acknowledgement for
// incoming messages (needed to keep the linked session healthy); that is not
// something a person sees as "read".

func readOnlyRefusal() *Refusal {
	return refuse("read_only", "This helper is read-only: it reads WhatsApp and never sends, marks messages as read, or shows that it is typing.")
}

// SetReadOnly turns read-only mode on. Call it before Start.
func (m *Manager) SetReadOnly(on bool) {
	m.readOnly = on
	if on {
		if _, already := m.backend.(*readOnlyBackend); !already {
			m.backend = &readOnlyBackend{inner: m.backend}
		}
	}
}

// ReadOnly reports whether read-only mode is on.
func (m *Manager) ReadOnly() bool { return m.readOnly }

type readOnlyBackend struct{ inner Backend }

func (b *readOnlyBackend) NewClient() Client { return &readOnlyClient{Client: b.inner.NewClient()} }

func (b *readOnlyBackend) Existing(ctx context.Context) ([]Client, error) {
	cs, err := b.inner.Existing(ctx)
	for i := range cs {
		cs[i] = &readOnlyClient{Client: cs[i]}
	}
	return cs, err
}

type readOnlyClient struct{ Client }

func (c *readOnlyClient) Send(context.Context, types.JID, *waE2E.Message) (string, time.Time, error) {
	return "", time.Time{}, readOnlyRefusal()
}

func (c *readOnlyClient) MarkRead(context.Context, []string, time.Time, types.JID, types.JID) error {
	return readOnlyRefusal()
}

func (c *readOnlyClient) SetTyping(context.Context, types.JID, bool) error { return nil }

func (c *readOnlyClient) Upload(context.Context, []byte, whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return whatsmeow.UploadResponse{}, readOnlyRefusal()
}
