// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

package main

import (
	"context"
	"database/sql"
	"runtime"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCompanionReg"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/store"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"
)

// Client is everything this program asks of one WhatsApp linked-device
// session. The real one is whatsmeow; tests use a fake that never opens a
// socket, so no test can link a real number or send a real message.
type Client interface {
	SetHandler(func(evt any))
	Connect() error
	Disconnect()
	IsConnected() bool
	JID() types.JID // empty until linked
	PushName() string
	PairPhone(ctx context.Context, phone string) (string, error)
	Send(ctx context.Context, to types.JID, msg *waE2E.Message) (id string, at time.Time, err error)
	MarkRead(ctx context.Context, ids []string, at time.Time, chat, sender types.JID) error
	SetTyping(ctx context.Context, chat types.JID, typing bool) error
	Upload(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error)
	Download(ctx context.Context, msg *waE2E.Message) ([]byte, error)
	OnWhatsApp(ctx context.Context, phone string) (types.JID, bool, error)
	Groups(ctx context.Context) (map[types.JID]string, error)
	Contacts(ctx context.Context) (map[types.JID]types.ContactInfo, error)
	ParseWebMessage(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error)
	PhoneJID(ctx context.Context, jid types.JID) types.JID // a hidden id's phone-number id, when known
	Logout(ctx context.Context) error                      // unlink on WhatsApp's side and delete the keys
	Forget(ctx context.Context) error                      // delete the keys only
}

// Backend makes and finds sessions.
type Backend interface {
	NewClient() Client
	Existing(ctx context.Context) ([]Client, error)
}

// ---- the real backend -------------------------------------------------------

type meowBackend struct{ container *sqlstore.Container }

func openMeow(ctx context.Context, path string) (*meowBackend, error) {
	db, err := sql.Open("sqlite", sqliteDSN(path))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	c := sqlstore.NewWithDB(db, "sqlite3", waLog.Noop)
	if err := c.Upgrade(ctx); err != nil {
		return nil, err
	}
	// What the customer sees under Linked devices on their phone.
	store.DeviceProps.Os = proto.String("Owlka")
	store.DeviceProps.PlatformType = waCompanionReg.DeviceProps_DESKTOP.Enum()
	return &meowBackend{container: c}, nil
}

func (b *meowBackend) NewClient() Client { return wrap(b.container.NewDevice()) }

func (b *meowBackend) Existing(ctx context.Context) ([]Client, error) {
	devices, err := b.container.GetAllDevices(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]Client, 0, len(devices))
	for _, d := range devices {
		out = append(out, wrap(d))
	}
	return out, nil
}

type meowClient struct {
	cli     *whatsmeow.Client
	handler uint32
}

func wrap(d *store.Device) *meowClient {
	return &meowClient{cli: whatsmeow.NewClient(d, waLog.Noop)}
}

func (m *meowClient) SetHandler(fn func(evt any)) {
	if m.handler != 0 {
		m.cli.RemoveEventHandler(m.handler)
	}
	m.handler = m.cli.AddEventHandler(func(evt any) { fn(evt) })
}

func (m *meowClient) Connect() error    { return m.cli.Connect() }
func (m *meowClient) Disconnect()       { m.cli.Disconnect() }
func (m *meowClient) IsConnected() bool { return m.cli.IsConnected() && m.cli.IsLoggedIn() }
func (m *meowClient) PushName() string  { return m.cli.Store.PushName }

func (m *meowClient) JID() types.JID {
	if m.cli.Store.ID == nil {
		return types.EmptyJID
	}
	return m.cli.Store.ID.ToNonAD()
}

func (m *meowClient) PairPhone(ctx context.Context, phone string) (string, error) {
	// WhatsApp only accepts a display name of the form "Browser (OS)".
	name := "Chrome (Mac OS)"
	if runtime.GOOS == "windows" {
		name = "Chrome (Windows)"
	}
	return m.cli.PairPhone(ctx, phone, true, whatsmeow.PairClientChrome, name)
}

func (m *meowClient) Send(ctx context.Context, to types.JID, msg *waE2E.Message) (string, time.Time, error) {
	resp, err := m.cli.SendMessage(ctx, to, msg)
	return string(resp.ID), resp.Timestamp, err
}

func (m *meowClient) MarkRead(ctx context.Context, ids []string, at time.Time, chat, sender types.JID) error {
	mids := make([]types.MessageID, len(ids))
	for i, id := range ids {
		mids[i] = types.MessageID(id)
	}
	return m.cli.MarkRead(ctx, mids, at, chat, sender)
}

// SetTyping shows or hides "typing". The session is marked online only for
// the moment of a send: a linked device that stays online stops the phone
// getting its own notifications.
func (m *meowClient) SetTyping(ctx context.Context, chat types.JID, typing bool) error {
	if typing {
		if err := m.cli.SendPresence(ctx, types.PresenceAvailable); err != nil {
			return err
		}
		return m.cli.SendChatPresence(ctx, chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	}
	err := m.cli.SendChatPresence(ctx, chat, types.ChatPresencePaused, types.ChatPresenceMediaText)
	if err2 := m.cli.SendPresence(ctx, types.PresenceUnavailable); err == nil {
		err = err2
	}
	return err
}

func (m *meowClient) Upload(ctx context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	return m.cli.Upload(ctx, data, kind)
}

func (m *meowClient) Download(ctx context.Context, msg *waE2E.Message) ([]byte, error) {
	return m.cli.DownloadAny(ctx, msg)
}

func (m *meowClient) OnWhatsApp(ctx context.Context, phone string) (types.JID, bool, error) {
	resp, err := m.cli.IsOnWhatsApp(ctx, []string{"+" + phone})
	if err != nil {
		return types.EmptyJID, false, err
	}
	for _, r := range resp {
		if r.IsIn {
			return r.JID.ToNonAD(), true, nil
		}
	}
	return types.EmptyJID, false, nil
}

func (m *meowClient) Groups(ctx context.Context) (map[types.JID]string, error) {
	groups, err := m.cli.GetJoinedGroups(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[types.JID]string, len(groups))
	for _, g := range groups {
		out[g.JID] = g.Name
	}
	return out, nil
}

func (m *meowClient) Contacts(ctx context.Context) (map[types.JID]types.ContactInfo, error) {
	return m.cli.Store.Contacts.GetAllContacts(ctx)
}

func (m *meowClient) ParseWebMessage(chat types.JID, msg *waWeb.WebMessageInfo) (*events.Message, error) {
	return m.cli.ParseWebMessage(chat, msg)
}

func (m *meowClient) PhoneJID(ctx context.Context, jid types.JID) types.JID {
	if jid.Server != types.HiddenUserServer {
		return jid
	}
	pn, err := m.cli.Store.LIDs.GetPNForLID(ctx, jid)
	if err != nil || pn.IsEmpty() {
		return jid
	}
	return pn.ToNonAD()
}

func (m *meowClient) Logout(ctx context.Context) error {
	err := m.cli.Logout(ctx)
	if err != nil {
		// Could not reach WhatsApp to unlink. Still remove the keys from this
		// computer; the phone will show the device until it is removed there.
		m.cli.Disconnect()
		if derr := m.cli.Store.Delete(ctx); derr != nil {
			return derr
		}
	}
	return err
}

func (m *meowClient) Forget(ctx context.Context) error {
	m.cli.Disconnect()
	return m.cli.Store.Delete(ctx)
}
