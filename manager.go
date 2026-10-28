// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

const (
	linkLifetime = 160 * time.Second // WhatsApp closes the linking socket after this
	// WhatsApp hands out several QR payloads at once. The first is good for
	// 60 seconds and each later one for 20; after the last the server closes
	// the socket. It is the client's job to move from one to the next.
	qrFirst      = 60 * time.Second
	qrEach       = 20 * time.Second
	maxFileBytes = 50 << 20
)

// account is one linked number.
type account struct {
	id     string // the number's digits; how callers name it
	client Client

	mu         sync.Mutex
	state      string // connecting | connected | logged_out | restricted | outdated
	detail     string
	sendMu     sync.Mutex // one send at a time per number
	lastSend   time.Time
	unrecorded []SendRecord // sends the store failed to record; still counted
}

func (a *account) set(state, detail string) {
	a.mu.Lock()
	a.state, a.detail = state, detail
	a.mu.Unlock()
}

type AccountInfo struct {
	Account string `json:"account"`
	Number  string `json:"number"`
	Name    string `json:"name,omitempty"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
	Usable  bool   `json:"usable"`
}

func (a *account) info() AccountInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	state := a.state
	if state == "connected" && !a.client.IsConnected() {
		state = "connecting"
	}
	return AccountInfo{Account: a.id, Number: "+" + a.id, Name: a.client.PushName(),
		State: state, Detail: a.detail, Usable: state == "connected"}
}

// link is one linking attempt in progress.
type link struct {
	id      string
	phone   string
	client  Client
	expires time.Time

	mu     sync.Mutex
	state  string // pending | linked | failed
	err    string
	code   string
	codes  []string // every QR payload WhatsApp offered, shown one at a time
	qrAt   time.Time
	acct   string
	closed bool
}

type Manager struct {
	backend Backend
	store   *Store
	dir     string
	limits  Limits

	// Seams for tests: time, waiting, and the jitter source.
	now    func() time.Time
	sleep  func(context.Context, time.Duration)
	jitter func() float64

	mu       sync.Mutex
	accounts map[string]*account
	links    map[string]*link
}

func NewManager(backend Backend, store *Store, dir string) *Manager {
	return &Manager{
		backend: backend, store: store, dir: dir, limits: DefaultLimits,
		now: time.Now,
		sleep: func(ctx context.Context, d time.Duration) {
			select {
			case <-ctx.Done():
			case <-time.After(d):
			}
		},
		jitter:   mrand.Float64,
		accounts: map[string]*account{},
		links:    map[string]*link{},
	}
}

// Start connects every number already linked on this computer.
func (m *Manager) Start(ctx context.Context) error {
	clients, err := m.backend.Existing(ctx)
	if err != nil {
		return err
	}
	for _, c := range clients {
		if c.JID().IsEmpty() {
			continue
		}
		m.adopt(c)
	}
	return nil
}

func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, a := range m.accounts {
		a.client.Disconnect()
	}
	for _, l := range m.links {
		l.client.Disconnect()
	}
}

// adopt registers a linked session as an account and connects it.
func (m *Manager) adopt(c Client) *account {
	a := &account{id: c.JID().User, client: c, state: "connecting"}
	m.mu.Lock()
	old := m.accounts[a.id]
	m.accounts[a.id] = a
	m.mu.Unlock()
	if old != nil && old.client != c {
		old.client.Disconnect()
	}
	c.SetHandler(func(evt any) { m.handle(a, evt) })
	if !c.IsConnected() {
		if err := c.Connect(); err != nil {
			a.set("connecting", "Could not reach WhatsApp yet. It retries by itself.")
		}
	} else {
		a.set("connected", "")
		go m.refreshNames(a)
	}
	return a
}

func (m *Manager) Accounts() []AccountInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]AccountInfo, 0, len(m.accounts))
	for _, a := range m.accounts {
		out = append(out, a.info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out
}

func (m *Manager) account(id string) (*account, *Refusal) {
	id = digits(id)
	m.mu.Lock()
	a := m.accounts[id]
	m.mu.Unlock()
	if a == nil {
		return nil, refuse("no_account", "The number +%s is not linked on this computer.", id)
	}
	return a, nil
}

// usable is the gate for anything that talks to WhatsApp.
func (a *account) usable() *Refusal {
	i := a.info()
	if i.Usable {
		return nil
	}
	switch i.State {
	case "logged_out":
		return refuse("logged_out", "WhatsApp has unlinked this computer from +%s. %s Link it again in the Owlka app.", a.id, i.Detail)
	case "restricted":
		return refuse("restricted", "WhatsApp has temporarily restricted +%s. %s Nothing was sent.", a.id, i.Detail)
	case "outdated":
		return refuse("outdated", "WhatsApp no longer accepts this version of the helper. Update Owlka.")
	}
	return refuse("offline", "+%s is not connected to WhatsApp right now. It reconnects by itself; try again in a moment.", a.id)
}

// ---- linking ---------------------------------------------------------------

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// StartLink begins linking a number and returns the 8-character code the
// person enters in WhatsApp. The socket stays open in this process while they
// do that.
func (m *Manager) StartLink(ctx context.Context, phone string) (*link, *Refusal) {
	num := digits(phone)
	qrOnly := strings.TrimSpace(phone) == ""
	if !qrOnly && (len(num) < 8 || strings.HasPrefix(num, "0")) {
		return nil, refuse("bad_number",
			"Enter the full number with its country code, for example +44 7700 900123.")
	}
	l := &link{id: newID(), phone: num, client: m.backend.NewClient(), state: "pending",
		expires: m.now().Add(linkLifetime)}
	ready := make(chan struct{}, 1)
	l.client.SetHandler(func(evt any) { m.handleLink(l, evt, ready) })
	if err := l.client.Connect(); err != nil {
		return nil, refuse("offline", "Could not reach WhatsApp. Check this computer's internet connection and try again.")
	}
	// The connection is ready for a pairing request once WhatsApp has offered
	// its first QR code.
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
	case <-ctx.Done():
	}
	lifetime := linkLifetime
	if qrOnly {
		// Linking by QR code: no number, no pairing code. The phone scans
		// what is on this screen, as it does for WhatsApp Web.
		l.mu.Lock()
		n := len(l.codes)
		l.mu.Unlock()
		if n == 0 {
			l.client.Disconnect()
			return nil, refuse("offline", "WhatsApp did not offer a QR code. Check this computer's internet connection and try again.")
		}
		lifetime = qrFirst + time.Duration(n-1)*qrEach
		l.mu.Lock()
		l.expires = l.qrAt.Add(lifetime)
		l.mu.Unlock()
	} else {
		code, err := l.client.PairPhone(ctx, num)
		if err != nil {
			l.client.Disconnect()
			return nil, refuse("link_failed", "WhatsApp did not accept the request to link +%s. Check the number and try again.", num)
		}
		if len(code) == 8 {
			code = code[:4] + "-" + code[4:]
		}
		l.mu.Lock()
		l.code = code
		l.mu.Unlock()
	}
	m.mu.Lock()
	m.links[l.id] = l
	m.mu.Unlock()
	// Give up when WhatsApp would: it closes the linking socket itself.
	time.AfterFunc(lifetime, func() { m.endLink(l, "failed", expiredMsg) })
	return l, nil
}

const expiredMsg = "The code ran out before it was entered. Start again to get a new one."

func (m *Manager) endLink(l *link, state, msg string) {
	l.mu.Lock()
	if l.state != "pending" {
		l.mu.Unlock()
		return
	}
	l.state, l.err = state, msg
	l.mu.Unlock()
	if state == "failed" {
		l.client.Disconnect()
		_ = l.client.Forget(context.Background())
	}
}

func (m *Manager) handleLink(l *link, evt any, ready chan struct{}) {
	switch e := evt.(type) {
	case *events.QR:
		if len(e.Codes) > 0 {
			l.mu.Lock()
			l.codes, l.qrAt = append([]string(nil), e.Codes...), m.now()
			l.mu.Unlock()
		}
		select {
		case ready <- struct{}{}:
		default:
		}
	case *events.PairSuccess:
		l.mu.Lock()
		l.acct = e.ID.User
		l.mu.Unlock()
	case *events.PairError:
		m.endLink(l, "failed", "WhatsApp refused to link this computer. Start again.")
	case *events.Connected:
		// Linked and logged in: this session is now an account.
		if l.client.JID().IsEmpty() {
			return
		}
		a := m.adopt(l.client)
		a.set("connected", "")
		go m.refreshNames(a)
		l.mu.Lock()
		l.state, l.acct = "linked", a.id
		l.mu.Unlock()
	case *events.ClientOutdated:
		m.endLink(l, "failed", "WhatsApp no longer accepts this version of the helper. Update Owlka.")
	}
}

type LinkStatus struct {
	LinkID    string       `json:"link_id"`
	State     string       `json:"state"`
	Code      string       `json:"code,omitempty"`
	QR        string       `json:"qr,omitempty"`
	ExpiresIn int          `json:"expires_in"`
	Error     string       `json:"error,omitempty"`
	Account   *AccountInfo `json:"account,omitempty"`
}

// currentQR is the QR payload that is valid now: WhatsApp's first for 60
// seconds, then a new one every 20. Empty before any was offered and after the
// last has run out. The caller holds l.mu.
func (l *link) currentQR(now time.Time) string {
	if len(l.codes) == 0 {
		return ""
	}
	i := 0
	if el := now.Sub(l.qrAt); el >= qrFirst {
		i = 1 + int((el-qrFirst)/qrEach)
	}
	if i >= len(l.codes) {
		return ""
	}
	return l.codes[i]
}

func (m *Manager) LinkStatus(id string) (*LinkStatus, *Refusal) {
	m.mu.Lock()
	l := m.links[id]
	m.mu.Unlock()
	if l == nil {
		return nil, refuse("no_link", "That linking attempt has ended. Start again.")
	}
	if !m.now().Before(l.expires) {
		m.endLink(l, "failed", expiredMsg)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	s := &LinkStatus{LinkID: l.id, State: l.state, Error: l.err}
	if l.state == "pending" {
		s.Code, s.QR = l.code, l.currentQR(m.now())
		if left := int(l.expires.Sub(m.now()).Seconds()); left > 0 {
			s.ExpiresIn = left
		}
	}
	if l.state == "linked" {
		m.mu.Lock()
		a := m.accounts[l.acct]
		m.mu.Unlock()
		if a != nil {
			i := a.info()
			s.Account = &i
		}
	}
	return s, nil
}

// Remove unlinks a number and deletes everything stored for it here.
func (m *Manager) Remove(ctx context.Context, id string) (string, *Refusal) {
	a, r := m.account(id)
	if r != nil {
		return "", r
	}
	note := "Unlinked from WhatsApp and removed from this computer."
	if err := a.client.Logout(ctx); err != nil {
		note = "Removed from this computer. WhatsApp could not be reached to unlink it, so also remove " +
			"\"Owlka\" under Linked devices on the phone."
	}
	m.mu.Lock()
	delete(m.accounts, a.id)
	m.mu.Unlock()
	if err := m.store.DeleteAccount(a.id); err != nil {
		return "", refuse("store", "The stored messages could not be deleted: %v", err)
	}
	_ = os.RemoveAll(filepath.Join(m.dir, "media", a.id))
	return note, nil
}

// ---- events ----------------------------------------------------------------

func (m *Manager) handle(a *account, evt any) {
	switch e := evt.(type) {
	case *events.Connected:
		a.set("connected", "")
		go m.refreshNames(a)
	case *events.Disconnected, *events.KeepAliveTimeout:
		a.set("connecting", "")
	case *events.LoggedOut:
		a.set("logged_out", "This happens when the device is removed on the phone, or the phone has been offline for about two weeks.")
	case *events.StreamReplaced:
		a.set("connecting", "Another copy of this helper took over the connection.")
	case *events.TemporaryBan:
		a.set("restricted", fmt.Sprintf("WhatsApp gave the reason %q and a wait of %s.", e.Code.String(), e.Expire.Round(time.Minute)))
	case *events.ClientOutdated:
		a.set("outdated", "")
	case *events.Message:
		m.logErr("store message", m.ingest(a, e, true))
	case *events.HistorySync:
		m.ingestHistory(a, e)
	case *events.Receipt:
		// Read on the phone or another device: no longer unread here.
		if e.Type == types.ReceiptTypeReadSelf {
			m.logErr("clear unread", m.store.SetUnread(a.id, m.chatID(a, e.Chat), 0))
		}
	case *events.MarkChatAsRead:
		if e.Action.GetRead() {
			m.logErr("clear unread", m.store.SetUnread(a.id, m.chatID(a, e.JID), 0))
		}
	case *events.Contact:
		name := e.Action.GetFullName()
		if name == "" {
			name = e.Action.GetFirstName()
		}
		m.logErr("store contact", m.store.PutContact(a.id, m.chatID(a, e.JID), name, ""))
	case *events.PushName:
		m.logErr("store name", m.store.PutContact(a.id, m.chatID(a, e.JID), "", e.NewPushName))
	}
}

// logErr records a failure to keep local history. Operation only: never a
// message, a name or a number.
func (m *Manager) logErr(op string, err error) {
	if err != nil {
		log.Printf("%s failed: %T", op, err)
	}
}

func (m *Manager) chatID(a *account, jid types.JID) string {
	return a.client.PhoneJID(context.Background(), jid.ToNonAD()).String()
}

func (m *Manager) refreshNames(a *account) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if groups, err := a.client.Groups(ctx); err == nil {
		for jid, name := range groups {
			m.logErr("store group", m.store.EnsureChat(a.id, jid.String(), name, true))
		}
	} else {
		m.logErr("list groups", err)
	}
	if contacts, err := a.client.Contacts(ctx); err == nil {
		for jid, c := range contacts {
			name := c.FullName
			if name == "" {
				name = c.FirstName
			}
			if name == "" {
				name = c.BusinessName
			}
			m.logErr("store contact", m.store.PutContact(a.id, m.chatID(a, jid), name, c.PushName))
		}
	} else {
		m.logErr("list contacts", err)
	}
}

func (m *Manager) ingestHistory(a *account, e *events.HistorySync) {
	for _, conv := range e.Data.GetConversations() {
		jid, err := types.ParseJID(conv.GetID())
		if err != nil || !isChat(jid) {
			continue
		}
		chat := m.chatID(a, jid)
		name := conv.GetName()
		if name == "" {
			name = conv.GetDisplayName()
		}
		m.logErr("store chat", m.store.EnsureChat(a.id, chat, name, jid.Server == types.GroupServer))
		m.logErr("set unread", m.store.SetUnread(a.id, chat, int(conv.GetUnreadCount())))
		for _, hm := range conv.GetMessages() {
			evt, err := a.client.ParseWebMessage(jid, hm.GetMessage())
			if err != nil {
				continue
			}
			m.logErr("store message", m.ingest(a, evt, false))
		}
	}
	for _, p := range e.Data.GetPushnames() {
		if jid, err := types.ParseJID(p.GetID()); err == nil && p.GetPushname() != "" {
			m.logErr("store name", m.store.PutContact(a.id, m.chatID(a, jid), "", p.GetPushname()))
		}
	}
}

// isChat is true for a person or a group: not a status update, a broadcast
// list or a channel.
func isChat(jid types.JID) bool {
	switch jid.Server {
	case types.DefaultUserServer, types.HiddenUserServer, types.GroupServer:
		return jid.User != "" && jid != types.StatusBroadcastJID
	}
	return false
}

// describe turns a WhatsApp message into what is stored: its kind, its text,
// and for media its type and file name.
func describe(msg *waE2E.Message) (kind, text, quoted, mimeType, fileName string) {
	switch {
	case msg.GetConversation() != "":
		return "text", msg.GetConversation(), "", "", ""
	case msg.GetExtendedTextMessage() != nil:
		x := msg.GetExtendedTextMessage()
		return "text", x.GetText(), x.GetContextInfo().GetStanzaID(), "", ""
	case msg.GetImageMessage() != nil:
		x := msg.GetImageMessage()
		return "image", x.GetCaption(), x.GetContextInfo().GetStanzaID(), x.GetMimetype(), ""
	case msg.GetVideoMessage() != nil:
		x := msg.GetVideoMessage()
		return "video", x.GetCaption(), x.GetContextInfo().GetStanzaID(), x.GetMimetype(), ""
	case msg.GetAudioMessage() != nil:
		x := msg.GetAudioMessage()
		kind = "audio"
		if x.GetPTT() {
			kind = "voice note"
		}
		return kind, "", x.GetContextInfo().GetStanzaID(), x.GetMimetype(), ""
	case msg.GetDocumentMessage() != nil:
		x := msg.GetDocumentMessage()
		return "file", x.GetCaption(), x.GetContextInfo().GetStanzaID(), x.GetMimetype(), x.GetFileName()
	case msg.GetStickerMessage() != nil:
		return "sticker", "", "", msg.GetStickerMessage().GetMimetype(), ""
	case msg.GetLocationMessage() != nil:
		x := msg.GetLocationMessage()
		return "location", strings.TrimSpace(fmt.Sprintf("%s %s (%.5f, %.5f)", x.GetName(), x.GetAddress(),
			x.GetDegreesLatitude(), x.GetDegreesLongitude())), "", "", ""
	case msg.GetContactMessage() != nil:
		return "contact card", msg.GetContactMessage().GetDisplayName(), "", "", ""
	}
	return "", "", "", "", ""
}

// ingest stores one message. live is false for history, which must not count
// as new unread messages.
func (m *Manager) ingest(a *account, e *events.Message, live bool) error {
	if e == nil || e.Message == nil || !isChat(e.Info.Chat) {
		return nil
	}
	chat := m.chatID(a, e.Info.Chat)
	sender := m.chatID(a, e.Info.Sender)

	if p := e.Message.GetProtocolMessage(); p != nil {
		switch p.GetType() {
		case waE2E.ProtocolMessage_REVOKE:
			return m.store.ReplaceText(a.id, chat, p.GetKey().GetID(), "deleted", "[This message was deleted]")
		case waE2E.ProtocolMessage_MESSAGE_EDIT:
			if _, text, _, _, _ := describe(p.GetEditedMessage()); text != "" {
				return m.store.ReplaceText(a.id, chat, p.GetKey().GetID(), "text", text)
			}
		}
		return nil
	}

	kind, text, quoted, mimeType, fileName := describe(e.Message)
	if kind == "" {
		return nil // reactions, polls, calls and system messages are not kept
	}
	row := Message{Chat: chat, ID: string(e.Info.ID), Sender: sender, SenderName: e.Info.PushName,
		FromMe: e.Info.IsFromMe, Kind: kind, Text: text, QuotedID: quoted,
		MediaMime: mimeType, MediaName: fileName, ts: e.Info.Timestamp}
	if e.IsViewOnce {
		// A view-once photo is meant to be seen once on the phone. Keep the
		// fact that it arrived, never the means to open it.
		row.Kind, row.Text, row.MediaMime, row.MediaName = "view once", "[View-once message. Open it on your phone.]", "", ""
	} else if mimeType != "" {
		// Kept so the file can be fetched later, on request. The file itself
		// is not downloaded now.
		raw, err := proto.Marshal(e.Message)
		if err != nil {
			return err
		}
		row.raw = raw
	}
	if err := m.store.EnsureChat(a.id, chat, "", e.Info.IsGroup); err != nil {
		return err
	}
	if !e.Info.IsFromMe && e.Info.PushName != "" {
		if err := m.store.PutContact(a.id, sender, "", e.Info.PushName); err != nil {
			return err
		}
	}
	fresh, err := m.store.PutMessage(a.id, row)
	if err != nil {
		return err
	}
	if fresh && live && !e.Info.IsFromMe {
		return m.store.AddUnread(a.id, chat)
	}
	return nil
}

// ---- reading ---------------------------------------------------------------

// ResolveChat turns what the caller gave (an id, a number or a name) into a
// chat id this account knows. It never invents one.
func (m *Manager) ResolveChat(a *account, who string) (string, *Refusal) {
	who = strings.TrimSpace(who)
	if who == "" {
		return "", refuse("no_chat", "Say which chat: a name, a number, or a chat id from the chat list.")
	}
	if strings.Contains(who, "@") {
		jid, err := types.ParseJID(who)
		if err != nil || !isChat(jid) {
			return "", refuse("bad_chat", "%q is not a chat that can be read or written to.", who)
		}
		return jid.ToNonAD().String(), nil
	}
	if looksLikeNumber(who) {
		return types.NewJID(digits(who), types.DefaultUserServer).String(), nil
	}
	found, err := m.store.People(a.id, who, true, 6)
	if err != nil {
		return "", refuse("store", "The local message store could not be read: %v", err)
	}
	switch len(found) {
	case 1:
		return found[0].JID, nil
	case 0:
		return "", refuse("no_chat", "No chat or contact is called %q. Search the contacts to find the right name.", who)
	}
	names := make([]string, len(found))
	for i, c := range found {
		names[i] = fmt.Sprintf("%s (%s)", c.Name, c.JID)
	}
	return "", refuse("ambiguous", "More than one chat is called %q: %s. Use the chat id.", who, strings.Join(names, ", "))
}

// ---- sending ---------------------------------------------------------------

type SendRequest struct {
	Account          string `json:"account"`
	To               string `json:"to"`
	Text             string `json:"text"`
	ReplyTo          string `json:"reply_to"`
	File             string `json:"file"`
	ConfirmNewNumber string `json:"confirm_new_number"`
	// Approved is set by the caller only when the person has explicitly
	// approved THIS send in their own words (Owlka's approval check decides
	// that, and writes a one-use grant the connector reads; the assistant has
	// no argument that sets it). It lifts the bulk and new-number rules. It
	// never lifts the pacing: see CheckSend.
	Approved bool `json:"approved"`
}

type SendResult struct {
	Chat      string `json:"chat"`
	ID        string `json:"id"`
	Time      string `json:"time"`
	NewNumber bool   `json:"new_number"`
}

// Send sends ONE message to ONE chat. There is no way to give it a list.
func (m *Manager) Send(ctx context.Context, req SendRequest) (*SendResult, *Refusal) {
	a, r := m.account(req.Account)
	if r != nil {
		return nil, r
	}
	if req.Text == "" && req.File == "" {
		return nil, refuse("empty", "There is nothing to send: give the message text or a file.")
	}
	chat, r := m.ResolveChat(a, req.To)
	if r != nil {
		return nil, r
	}
	to, _ := types.ParseJID(chat)

	// RULE: only a chat that already exists, or a number the person named.
	known, err := m.store.HasChat(a.id, chat)
	if err != nil {
		return nil, refuse("store", "The local message store could not be read: %v", err)
	}
	newNumber := false
	if !known {
		if to.Server != types.DefaultUserServer {
			return nil, refuse("unknown_chat", "%s is not a chat this number is in, so nothing was sent.", chat)
		}
		if !req.Approved && digits(req.ConfirmNewNumber) != to.User {
			return nil, refuse("unknown_number",
				"There is no existing chat with +%s, so nothing was sent. A first message to a new number is only sent "+
					"when the person has asked for that exact number: repeat it in confirm_new_number. "+
					"Or ask the person to confirm this send in so many words, then send it again.", to.User)
		}
		newNumber = true
	}
	if r := a.usable(); r != nil {
		return nil, r
	}

	var quoted *Message
	if req.ReplyTo != "" {
		quoted, err = m.store.Message(a.id, chat, req.ReplyTo)
		if err != nil || quoted == nil {
			return nil, refuse("no_message", "The message being replied to (%s) is not in this chat.", req.ReplyTo)
		}
	}
	var fileData []byte
	if req.File != "" {
		info, err := os.Stat(req.File)
		if err != nil || info.IsDir() {
			return nil, refuse("no_file", "The file %s could not be read.", req.File)
		}
		if info.Size() > maxFileBytes {
			return nil, refuse("file_too_big", "That file is %d MB. The limit is %d MB.", info.Size()>>20, maxFileBytes>>20)
		}
		if fileData, err = os.ReadFile(req.File); err != nil {
			return nil, refuse("no_file", "The file %s could not be read.", req.File)
		}
	}

	// One send at a time per number, so the limits cannot be raced.
	a.sendMu.Lock()
	defer a.sendMu.Unlock()

	// RULES: no bulk, and rate limits.
	record := SendRecord{Chat: chat, TextHash: TextHash(req.Text), NewNumber: newNumber}
	history, err := m.store.RecentSends(a.id, m.now())
	if err != nil {
		return nil, refuse("store", "The local message store could not be read: %v", err)
	}
	history = append(history, a.unrecorded...)
	if r := CheckSend(history, m.now(), record, m.limits, req.Approved); r != nil {
		return nil, r
	}
	if newNumber {
		jid, on, err := a.client.OnWhatsApp(ctx, to.User)
		if err != nil {
			return nil, refuse("offline", "WhatsApp could not be asked about +%s. Nothing was sent.", to.User)
		}
		if !on {
			return nil, refuse("not_on_whatsapp", "+%s is not on WhatsApp. Nothing was sent.", to.User)
		}
		to = jid
	}

	// RULE: paced like a person. Never back to back, and typing shows first.
	m.sleep(ctx, GapWait(a.lastSend, m.now(), m.limits))
	typingErr := a.client.SetTyping(ctx, to, true)
	typed := typingErr == nil
	// The wait below still happens, so the message is still paced; only the
	// "typing" indicator is missing. Recorded so it is not a silent downgrade.
	m.logErr("show typing", typingErr)
	m.sleep(ctx, TypingDelay(len(req.Text), m.jitter()))
	if typed {
		defer func() { _ = a.client.SetTyping(context.Background(), to, false) }()
	}
	if ctx.Err() != nil {
		return nil, refuse("cancelled", "The send was cancelled before the message went. Nothing was sent.")
	}

	msg, kind, mimeType, fileName, r := m.build(ctx, a, req, quoted, fileData)
	if r != nil {
		return nil, r
	}
	id, at, err := a.client.Send(ctx, to, msg)
	if err != nil {
		return nil, refuse("send_failed", "WhatsApp did not accept the message: %s. It may not have been sent.", sendError(err))
	}
	if at.IsZero() {
		at = m.now()
	}
	a.lastSend = m.now()
	record.At = a.lastSend
	if err := m.store.RecordSend(a.id, record); err != nil {
		// The message WENT. Dropping it from the count would loosen the
		// limits, so it is counted in memory instead.
		a.unrecorded = append(a.unrecorded, record)
		m.logErr("record send", err)
	}
	row := Message{Chat: to.String(), ID: id, Sender: a.client.JID().String(), FromMe: true, Kind: kind,
		Text: req.Text, QuotedID: req.ReplyTo, MediaMime: mimeType, MediaName: fileName, ts: at}
	m.logErr("store chat", m.store.EnsureChat(a.id, row.Chat, "", to.Server == types.GroupServer))
	_, err = m.store.PutMessage(a.id, row)
	m.logErr("store sent message", err)
	return &SendResult{Chat: row.Chat, ID: id, Time: stamp(at), NewNumber: newNumber}, nil
}

func sendError(err error) string {
	switch {
	case errors.Is(err, whatsmeow.ErrNotConnected), errors.Is(err, whatsmeow.ErrNotLoggedIn):
		return "this computer is not connected to WhatsApp"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, whatsmeow.ErrMessageTimedOut):
		return "WhatsApp did not answer in time"
	}
	text := err.Error()
	if len(text) > 160 {
		text = text[:160]
	}
	return text
}

func (m *Manager) build(ctx context.Context, a *account, req SendRequest, quoted *Message, data []byte) (
	msg *waE2E.Message, kind, mimeType, fileName string, r *Refusal) {
	var ci *waE2E.ContextInfo
	if quoted != nil {
		ci = &waE2E.ContextInfo{StanzaID: proto.String(quoted.ID), Participant: proto.String(quoted.Sender),
			QuotedMessage: &waE2E.Message{Conversation: proto.String(quoted.Text)}}
	}
	if data == nil {
		if ci == nil {
			return &waE2E.Message{Conversation: proto.String(req.Text)}, "text", "", "", nil
		}
		return &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{
			Text: proto.String(req.Text), ContextInfo: ci}}, "text", "", "", nil
	}
	fileName = filepath.Base(req.File)
	mimeType = mime.TypeByExtension(strings.ToLower(filepath.Ext(fileName)))
	if mimeType == "" {
		mimeType = http.DetectContentType(data)
	}
	mimeType, _, _ = strings.Cut(mimeType, ";")
	isImage := mimeType == "image/jpeg" || mimeType == "image/png"
	mediaKind := whatsmeow.MediaDocument
	if isImage {
		mediaKind = whatsmeow.MediaImage
	}
	up, err := a.client.Upload(ctx, data, mediaKind)
	if err != nil {
		return nil, "", "", "", refuse("upload_failed", "The file could not be uploaded to WhatsApp: %s. Nothing was sent.", sendError(err))
	}
	var caption *string
	if req.Text != "" {
		caption = proto.String(req.Text)
	}
	if isImage {
		return &waE2E.Message{ImageMessage: &waE2E.ImageMessage{
			URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
			Mimetype: proto.String(mimeType), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
			FileLength: proto.Uint64(up.FileLength), Caption: caption, ContextInfo: ci}}, "image", mimeType, fileName, nil
	}
	return &waE2E.Message{DocumentMessage: &waE2E.DocumentMessage{
		URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
		Mimetype: proto.String(mimeType), FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256,
		FileLength: proto.Uint64(up.FileLength), FileName: proto.String(fileName), Title: proto.String(fileName),
		Caption: caption, ContextInfo: ci}}, "file", mimeType, fileName, nil
}

// MarkRead tells WhatsApp the chat has been read (the other person sees blue
// ticks, if the account has read receipts on).
func (m *Manager) MarkRead(ctx context.Context, acct, who string) (int, string, *Refusal) {
	a, r := m.account(acct)
	if r != nil {
		return 0, "", r
	}
	chat, r := m.ResolveChat(a, who)
	if r != nil {
		return 0, "", r
	}
	if r := a.usable(); r != nil {
		return 0, "", r
	}
	msgs, err := m.store.LastIncoming(a.id, chat, 30)
	if err != nil {
		return 0, "", refuse("store", "The local message store could not be read: %v", err)
	}
	chatJID, _ := types.ParseJID(chat)
	bySender := map[string][]string{}
	for _, msg := range msgs {
		bySender[msg.Sender] = append(bySender[msg.Sender], msg.ID)
	}
	for sender, ids := range bySender {
		senderJID, _ := types.ParseJID(sender)
		if err := a.client.MarkRead(ctx, ids, m.now(), chatJID, senderJID); err != nil {
			return 0, "", refuse("send_failed", "WhatsApp did not accept the read receipt: %s.", sendError(err))
		}
	}
	if err := m.store.SetUnread(a.id, chat, 0); err != nil {
		return 0, "", refuse("store", "The chat was marked read on WhatsApp but not in the local store: %v", err)
	}
	return len(msgs), chat, nil
}

// Media downloads one message's file to this computer and returns its path.
func (m *Manager) Media(ctx context.Context, acct, who, id string) (string, *Message, *Refusal) {
	a, r := m.account(acct)
	if r != nil {
		return "", nil, r
	}
	chat, r := m.ResolveChat(a, who)
	if r != nil {
		return "", nil, r
	}
	msg, err := m.store.Message(a.id, chat, id)
	if err != nil || msg == nil {
		return "", nil, refuse("no_message", "Message %s is not in this chat.", id)
	}
	if len(msg.raw) == 0 {
		return "", nil, refuse("no_media", "That message has no file to fetch.")
	}
	ext := filepath.Ext(msg.MediaName)
	if ext == "" {
		if exts, _ := mime.ExtensionsByType(msg.MediaMime); len(exts) > 0 {
			ext = exts[0]
		}
	}
	dir := filepath.Join(m.dir, "media", a.id)
	path := filepath.Join(dir, safeName(id)+ext)
	if _, err := os.Stat(path); err == nil {
		return path, msg, nil
	}
	if r := a.usable(); r != nil {
		return "", nil, r
	}
	var wa waE2E.Message
	if err := proto.Unmarshal(msg.raw, &wa); err != nil {
		return "", nil, refuse("no_media", "That message's file details could not be read.")
	}
	data, err := a.client.Download(ctx, &wa)
	if err != nil {
		return "", nil, refuse("media_gone", "WhatsApp no longer has that file to download (it keeps files for a limited time). Open it on the phone instead.")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, refuse("store", "The file could not be saved: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", nil, refuse("store", "The file could not be saved: %v", err)
	}
	return path, msg, nil
}

func safeName(s string) string {
	return strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, s)
}
