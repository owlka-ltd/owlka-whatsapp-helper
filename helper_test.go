// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

package main

// These tests run the whole helper (HTTP API, rules, message store) against a
// FAKE WhatsApp session. No test opens a socket to WhatsApp, links a number or
// sends a message. The fake stands where the whatsmeow library stands, and is
// fed real whatsmeow event and message types.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waCommon"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/proto/waHistorySync"
	"go.mau.fi/whatsmeow/proto/waWeb"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
)

type sent struct {
	to  types.JID
	msg *waE2E.Message
}

type fakeClient struct {
	mu        sync.Mutex
	jid       types.JID
	connected bool
	handler   func(any)
	trace     *[]string // shared order-of-events log
	sent      []sent
	read      [][]string
	onWA      map[string]bool
	sendErr   error
	loggedOut bool
	forgotten bool
	media     []byte
}

func (f *fakeClient) note(s string) {
	if f.trace != nil {
		*f.trace = append(*f.trace, s)
	}
}
func (f *fakeClient) emit(evt any) {
	f.mu.Lock()
	h := f.handler
	f.mu.Unlock()
	if h != nil {
		h(evt)
	}
}
func (f *fakeClient) SetHandler(fn func(any)) { f.mu.Lock(); f.handler = fn; f.mu.Unlock() }
func (f *fakeClient) Connect() error {
	if f.jid.IsEmpty() {
		go f.emit(&events.QR{Codes: []string{"QR-PAYLOAD-1"}})
		return nil
	}
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	f.emit(&events.Connected{})
	return nil
}
func (f *fakeClient) Disconnect()       { f.mu.Lock(); f.connected = false; f.mu.Unlock() }
func (f *fakeClient) IsConnected() bool { f.mu.Lock(); defer f.mu.Unlock(); return f.connected }
func (f *fakeClient) JID() types.JID    { f.mu.Lock(); defer f.mu.Unlock(); return f.jid }
func (f *fakeClient) PushName() string  { return "Test Person" }
func (f *fakeClient) PairPhone(_ context.Context, phone string) (string, error) {
	if phone == "15550000000" {
		return "", errors.New("bad request")
	}
	return "ABCDEFGH", nil
}

// phoneEntersCode is the customer typing the code into WhatsApp.
func (f *fakeClient) phoneEntersCode(number string) {
	f.mu.Lock()
	f.jid = types.NewJID(number, types.DefaultUserServer)
	f.connected = true
	f.mu.Unlock()
	f.emit(&events.PairSuccess{ID: f.jid})
	f.emit(&events.Connected{})
}
func (f *fakeClient) Send(_ context.Context, to types.JID, msg *waE2E.Message) (string, time.Time, error) {
	if f.sendErr != nil {
		return "", time.Time{}, f.sendErr
	}
	f.note("send")
	f.sent = append(f.sent, sent{to, msg})
	return fmt.Sprintf("OUT%d", len(f.sent)), time.Time{}, nil
}
func (f *fakeClient) MarkRead(_ context.Context, ids []string, _ time.Time, _, _ types.JID) error {
	f.read = append(f.read, ids)
	return nil
}
func (f *fakeClient) SetTyping(_ context.Context, _ types.JID, typing bool) error {
	f.note(fmt.Sprintf("typing=%v", typing))
	return nil
}
func (f *fakeClient) Upload(_ context.Context, data []byte, kind whatsmeow.MediaType) (whatsmeow.UploadResponse, error) {
	f.note("upload:" + string(kind))
	return whatsmeow.UploadResponse{URL: "https://media.invalid/x", DirectPath: "/x", FileLength: uint64(len(data))}, nil
}
func (f *fakeClient) Download(context.Context, *waE2E.Message) ([]byte, error) {
	if f.media == nil {
		return nil, errors.New("gone")
	}
	return f.media, nil
}
func (f *fakeClient) OnWhatsApp(_ context.Context, phone string) (types.JID, bool, error) {
	return types.NewJID(phone, types.DefaultUserServer), f.onWA[phone], nil
}
func (f *fakeClient) Groups(context.Context) (map[types.JID]string, error) {
	return map[types.JID]string{types.NewJID("12036300000000", types.GroupServer): "Family"}, nil
}
func (f *fakeClient) Contacts(context.Context) (map[types.JID]types.ContactInfo, error) {
	return map[types.JID]types.ContactInfo{
		types.NewJID("447700900111", types.DefaultUserServer): {FullName: "Alice Example"},
		types.NewJID("447700900333", types.DefaultUserServer): {FullName: "Carol NoChat"},
	}, nil
}
func (f *fakeClient) ParseWebMessage(chat types.JID, m *waWeb.WebMessageInfo) (*events.Message, error) {
	sender := chat
	if m.GetKey().GetFromMe() {
		sender = f.JID()
	}
	return &events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: chat, Sender: sender, IsFromMe: m.GetKey().GetFromMe(),
			IsGroup: chat.Server == types.GroupServer},
		ID: m.GetKey().GetID(), Timestamp: time.Unix(int64(m.GetMessageTimestamp()), 0), PushName: m.GetPushName(),
	}, Message: m.GetMessage()}, nil
}
func (f *fakeClient) PhoneJID(_ context.Context, jid types.JID) types.JID {
	if jid.Server == types.HiddenUserServer && jid.User == "99001" { // Alice's hidden id
		return types.NewJID("447700900111", types.DefaultUserServer)
	}
	return jid
}
func (f *fakeClient) Logout(context.Context) error {
	f.loggedOut = true
	f.connected = false
	return nil
}
func (f *fakeClient) Forget(context.Context) error { f.forgotten = true; return nil }

type fakeBackend struct {
	existing []Client
	made     []*fakeClient
	trace    *[]string
}

func (b *fakeBackend) NewClient() Client {
	c := &fakeClient{trace: b.trace, onWA: map[string]bool{"447700900222": true}}
	b.made = append(b.made, c)
	return c
}
func (b *fakeBackend) Existing(context.Context) ([]Client, error) { return b.existing, nil }

const me = "447700900000"
const alice = "447700900111@s.whatsapp.net"

type rig struct {
	t      *testing.T
	m      *Manager
	client *fakeClient
	back   *fakeBackend
	srv    *httptest.Server
	clock  time.Time
	sleeps []time.Duration
	trace  []string
	dir    string
}

// newRig starts a helper with one linked number that has a chat with Alice.
func newRig(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, clock: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), dir: t.TempDir()}
	store, err := OpenStore(filepath.Join(r.dir, "messages.db"))
	if err != nil {
		t.Fatal(err)
	}
	r.client = &fakeClient{jid: types.NewJID(me, types.DefaultUserServer), trace: &r.trace,
		onWA: map[string]bool{"447700900222": true}, media: []byte("JPEGDATA")}
	r.back = &fakeBackend{existing: []Client{r.client}, trace: &r.trace}
	r.m = NewManager(r.back, store, r.dir)
	r.m.now = func() time.Time { return r.clock }
	r.m.sleep = func(_ context.Context, d time.Duration) {
		r.sleeps = append(r.sleeps, d)
		r.trace = append(r.trace, "wait")
		r.clock = r.clock.Add(d)
	}
	r.m.jitter = func() float64 { return 0.5 }
	if err := r.m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	r.srv = httptest.NewServer((&API{m: r.m, token: "TOKEN", stop: func() {}}).Handler())
	t.Cleanup(func() { r.srv.Close(); store.Close() })
	r.incoming(alice, "IN1", "Are we still on for Friday?")
	return r
}

func (r *rig) incoming(chat, id, text string) {
	jid, _ := types.ParseJID(chat)
	r.client.emit(&events.Message{Info: types.MessageInfo{
		MessageSource: types.MessageSource{Chat: jid, Sender: jid, IsGroup: jid.Server == types.GroupServer},
		ID:            id, Timestamp: r.clock, PushName: "Ali"},
		Message: &waE2E.Message{Conversation: proto.String(text)}})
	r.clock = r.clock.Add(time.Second)
}

func (r *rig) call(method, path string, body any) (int, map[string]any) {
	r.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else {
			_ = json.NewEncoder(&buf).Encode(body)
		}
	}
	req, _ := http.NewRequest(method, r.srv.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer TOKEN")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	out := map[string]any{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func (r *rig) send(body map[string]any) (int, string) {
	body["account"] = me
	status, out := r.call("POST", "/send", body)
	if e, ok := out["error"].(map[string]any); ok {
		return status, e["code"].(string) + ": " + e["message"].(string)
	}
	return status, ""
}

func (r *rig) nothingSent(why string) {
	r.t.Helper()
	if len(r.client.sent) != 0 {
		r.t.Fatalf("%s: %d message(s) went out", why, len(r.client.sent))
	}
}

// ---- PRODUCT RULE 2: no bulk or broadcast -----------------------------------

func TestRuleNoBulk_ASendTakesOneRecipientNeverAList(t *testing.T) {
	r := newRig(t)
	status, _ := r.call("POST", "/send", `{"account":"`+me+`","to":["`+alice+`","447700900222"],"text":"hi"}`)
	if status != 400 {
		t.Fatalf("a list of recipients was accepted (status %d)", status)
	}
	status, _ = r.call("POST", "/send", `{"account":"`+me+`","to":"`+alice+`","recipients":["x"],"text":"hi"}`)
	if status != 400 {
		t.Fatalf("an unknown 'recipients' field was accepted (status %d)", status)
	}
	r.nothingSent("list of recipients")
	for _, route := range []string{"/broadcast", "/send/bulk", "/send_many"} {
		if status, _ := r.call("POST", route, `{}`); status != 404 {
			t.Fatalf("%s exists (status %d): the helper must have no bulk route", route, status)
		}
	}
}

func TestRuleNoBulk_StatusBroadcastListsAndChannelsAreRefused(t *testing.T) {
	r := newRig(t)
	for _, to := range []string{"status@broadcast", "12345@broadcast", "12345@newsletter"} {
		if status, msg := r.send(map[string]any{"to": to, "text": "hi"}); status == 200 {
			t.Fatalf("%s was accepted: %s", to, msg)
		}
	}
	r.nothingSent("broadcast recipients")
}

func TestRuleNoBulk_TheSameTextToManyChatsIsRefused(t *testing.T) {
	r := newRig(t)
	chats := []string{alice}
	for i := 0; i < 4; i++ {
		c := fmt.Sprintf("44770090100%d@s.whatsapp.net", i)
		r.incoming(c, fmt.Sprintf("X%d", i), "hello")
		chats = append(chats, c)
	}
	var lastStatus int
	var lastMsg string
	for _, c := range chats {
		lastStatus, lastMsg = r.send(map[string]any{"to": c, "text": "Big  SALE today!"})
		r.clock = r.clock.Add(2 * time.Minute)
	}
	if got := len(r.client.sent); got != DefaultLimits.SameTextChatsPerHour {
		t.Fatalf("the same text reached %d chats; the limit is %d", got, DefaultLimits.SameTextChatsPerHour)
	}
	if lastStatus != 429 || !strings.HasPrefix(lastMsg, "broadcast:") {
		t.Fatalf("want a broadcast refusal, got %d %s", lastStatus, lastMsg)
	}
	// A different message to that chat is ordinary conversation.
	if status, msg := r.send(map[string]any{"to": chats[4], "text": "See you at 6"}); status != 200 {
		t.Fatalf("a different message was refused: %s", msg)
	}
}

// ---- PRODUCT RULE 3: paced like a person -----------------------------------

func TestRulePacing_TypingShowsAndTimePassesBeforeTheMessageGoes(t *testing.T) {
	r := newRig(t)
	r.trace = nil
	text := "Yes, see you Friday at seven."
	if status, msg := r.send(map[string]any{"to": alice, "text": text}); status != 200 {
		t.Fatal(msg)
	}
	got := strings.Join(r.trace, " ")
	if got != "wait typing=true wait send typing=false" {
		t.Fatalf("order was %q", got)
	}
	want := TypingDelay(len(text), 0.5)
	if r.sleeps[1] != want || want < 2*time.Second {
		t.Fatalf("typed for %v, want %v (and at least 2s for a sentence)", r.sleeps[1], want)
	}
}

func TestRulePacing_TypingTimeGrowsWithLengthAndIsBounded(t *testing.T) {
	if short, long := TypingDelay(5, 0.5), TypingDelay(120, 0.5); long <= short {
		t.Fatalf("a long message typed no slower (%v vs %v)", long, short)
	}
	for _, j := range []float64{0, 0.999} {
		if d := TypingDelay(0, j); d < 900*time.Millisecond {
			t.Fatalf("an empty message went after only %v", d)
		}
		if d := TypingDelay(100000, j); d > 12*time.Second {
			t.Fatalf("typing time is unbounded: %v", d)
		}
	}
}

func TestRulePacing_TwoSendsAreNeverBackToBack(t *testing.T) {
	r := newRig(t)
	r.send(map[string]any{"to": alice, "text": "one"})
	first := r.clock
	r.sleeps = nil
	r.send(map[string]any{"to": alice, "text": "two"})
	if r.sleeps[0] != DefaultLimits.MinGap {
		t.Fatalf("second send waited %v, want the %v gap", r.sleeps[0], DefaultLimits.MinGap)
	}
	if gap := r.clock.Sub(first); gap < DefaultLimits.MinGap {
		t.Fatalf("sends were %v apart", gap)
	}
}

func TestRulePacing_RateLimitsRefuseAndNeverQueue(t *testing.T) {
	r := newRig(t)
	r.m.limits.MinGap = 0
	r.m.jitter = func() float64 { return 0 }
	r.m.sleep = func(context.Context, time.Duration) {} // the clock stands still: all in one minute
	for i := 0; i < DefaultLimits.PerMinute; i++ {
		if status, msg := r.send(map[string]any{"to": alice, "text": fmt.Sprintf("message %d", i)}); status != 200 {
			t.Fatalf("send %d refused: %s", i, msg)
		}
	}
	status, msg := r.send(map[string]any{"to": alice, "text": "one too many"})
	if status != 429 || !strings.HasPrefix(msg, "rate_limited:") {
		t.Fatalf("want a rate-limit refusal, got %d %s", status, msg)
	}
	if len(r.client.sent) != DefaultLimits.PerMinute {
		t.Fatalf("%d sent, limit %d", len(r.client.sent), DefaultLimits.PerMinute)
	}
	r.clock = r.clock.Add(2 * time.Minute)
	if len(r.client.sent) != DefaultLimits.PerMinute {
		t.Fatal("the refused message went out later: refusals must not queue")
	}
	if status, msg := r.send(map[string]any{"to": alice, "text": "after waiting"}); status != 200 {
		t.Fatalf("still refused after the minute passed: %s", msg)
	}
}

func TestRulePacing_HourAndDayLimits(t *testing.T) {
	now := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	var hist []SendRecord
	for i := 0; i < DefaultLimits.PerHour; i++ {
		hist = append(hist, SendRecord{At: now.Add(-time.Duration(i+2) * 50 * time.Second), Chat: "a", TextHash: fmt.Sprint(i)})
	}
	if r := CheckSend(hist, now, SendRecord{Chat: "a", TextHash: "n"}, DefaultLimits); r == nil || r.Code != "rate_limited" {
		t.Fatalf("hour limit not applied: %v", r)
	}
	hist = nil
	for i := 0; i < DefaultLimits.PerDay; i++ {
		hist = append(hist, SendRecord{At: now.Add(-2*time.Hour - time.Duration(i)*time.Minute), Chat: "a", TextHash: fmt.Sprint(i)})
	}
	if r := CheckSend(hist, now, SendRecord{Chat: "a", TextHash: "n"}, DefaultLimits); r == nil || r.Code != "rate_limited" {
		t.Fatalf("day limit not applied: %v", r)
	}
	if r := CheckSend(hist[:10], now, SendRecord{Chat: "a", TextHash: "n"}, DefaultLimits); r != nil {
		t.Fatalf("ordinary use refused: %v", r)
	}
}

// ---- PRODUCT RULE 4: only a named number or an existing chat ----------------

func TestRuleKnownRecipient_ANumberWithNoChatIsRefused(t *testing.T) {
	r := newRig(t)
	for _, to := range []string{"+44 7700 900222", "447700900222@s.whatsapp.net", "Carol NoChat"} {
		status, msg := r.send(map[string]any{"to": to, "text": "hello"})
		if status != 403 || !strings.HasPrefix(msg, "unknown_number:") {
			t.Fatalf("%s: want unknown_number, got %d %s", to, status, msg)
		}
	}
	// Confirming a DIFFERENT number is not naming this one.
	if status, _ := r.send(map[string]any{"to": "+447700900222", "text": "hi", "confirm_new_number": "+447700900999"}); status != 403 {
		t.Fatal("a mismatched confirmation was accepted")
	}
	r.nothingSent("unknown numbers")
}

func TestRuleKnownRecipient_ANamedNumberIsSentAndCapped(t *testing.T) {
	r := newRig(t)
	status, msg := r.send(map[string]any{"to": "+44 7700 900222", "text": "Hi, Tim asked me to pass this on.",
		"confirm_new_number": "+44 7700 900222"})
	if status != 200 || len(r.client.sent) != 1 || r.client.sent[0].to.User != "447700900222" {
		t.Fatalf("a named number was not sent to: %d %s", status, msg)
	}
	// Now there is a chat, so the next message needs no confirmation.
	if status, msg := r.send(map[string]any{"to": "+447700900222", "text": "Second message"}); status != 200 {
		t.Fatalf("an existing chat needed confirming: %s", msg)
	}
	// Not on WhatsApp: refused, nothing sent.
	if status, msg := r.send(map[string]any{"to": "+447700900444", "text": "x", "confirm_new_number": "447700900444"}); status == 200 || !strings.Contains(msg, "not on WhatsApp") {
		t.Fatalf("want not-on-WhatsApp, got %d %s", status, msg)
	}
	// The daily cap on first messages to new numbers.
	for i := 0; i < 6; i++ {
		n := fmt.Sprintf("44770090200%d", i)
		r.client.onWA[n] = true
		r.clock = r.clock.Add(5 * time.Minute)
		status, msg = r.send(map[string]any{"to": n, "text": fmt.Sprintf("note %d", i), "confirm_new_number": n})
	}
	if status != 429 || !strings.HasPrefix(msg, "new_number_limit:") {
		t.Fatalf("want the new-number cap, got %d %s", status, msg)
	}
	newOnes := 0
	for _, s := range r.client.sent {
		if s.to.User != "447700900222" {
			newOnes++
		}
	}
	if newOnes+1 != DefaultLimits.NewNumbersPerDay {
		t.Fatalf("%d new numbers messaged in a day; the cap is %d", newOnes+1, DefaultLimits.NewNumbersPerDay)
	}
}

func TestRuleKnownRecipient_ExistingChatsAndGroupsSendWithoutConfirmation(t *testing.T) {
	r := newRig(t)
	waitFor(t, func() bool { ok, _ := r.m.store.HasChat(me, "12036300000000@g.us"); return ok })
	for _, to := range []string{alice, "+44 7700 900111", "Alice Example", "Family"} {
		if status, msg := r.send(map[string]any{"to": to, "text": "hello " + to}); status != 200 {
			t.Fatalf("%s refused: %s", to, msg)
		}
		r.clock = r.clock.Add(time.Minute)
	}
	if r.client.sent[3].to.Server != types.GroupServer {
		t.Fatalf("the group message went to %v", r.client.sent[3].to)
	}
	// A group this number is not in cannot be written to, confirmed or not.
	before := len(r.client.sent)
	status, msg := r.send(map[string]any{"to": "12036399999999@g.us", "text": "hi", "confirm_new_number": "12036399999999"})
	if status != 403 || len(r.client.sent) != before {
		t.Fatalf("an unknown group was written to: %d %s", status, msg)
	}
}

// ---- linking ---------------------------------------------------------------

func TestLinkingShowsACodeAndBecomesAnAccountWhenThePhoneEntersIt(t *testing.T) {
	r := newRig(t)
	status, out := r.call("POST", "/link/start", map[string]any{"phone": "+44 7700 900555"})
	if status != 200 || out["code"] != "ABCD-EFGH" || out["state"] != "pending" {
		t.Fatalf("start: %d %v", status, out)
	}
	if out["expires_in"].(float64) < 100 {
		t.Fatalf("code lifetime %v", out["expires_in"])
	}
	id := out["link_id"]
	_, out = r.call("POST", "/link/status", map[string]any{"link_id": id})
	if out["state"] != "pending" || out["qr"] != "QR-PAYLOAD-1" {
		t.Fatalf("status while waiting: %v", out)
	}
	r.back.made[0].phoneEntersCode("447700900555")
	_, out = r.call("POST", "/link/status", map[string]any{"link_id": id})
	acct, _ := out["account"].(map[string]any)
	if out["state"] != "linked" || acct["account"] != "447700900555" || acct["usable"] != true {
		t.Fatalf("after entering the code: %v", out)
	}
	if _, has := out["code"]; has {
		t.Fatal("the code is still offered after linking")
	}
	_, out = r.call("GET", "/accounts", nil)
	if n := len(out["accounts"].([]any)); n != 2 {
		t.Fatalf("%d accounts, want two numbers linked side by side", n)
	}
}

func TestLinkingRefusesABadNumberAndExpires(t *testing.T) {
	r := newRig(t)
	for _, phone := range []string{"", "07700 900123", "12345"} {
		if status, _ := r.call("POST", "/link/start", map[string]any{"phone": phone}); status != 400 {
			t.Fatalf("%q accepted (status %d)", phone, status)
		}
	}
	if len(r.back.made) != 0 {
		t.Fatal("a session was opened for a number that could be refused locally")
	}
	if status, _ := r.call("POST", "/link/start", map[string]any{"phone": "+15550000000"}); status == 200 {
		t.Fatal("a pairing request WhatsApp refused was reported as started")
	}
	_, out := r.call("POST", "/link/start", map[string]any{"phone": "+447700900555"})
	r.clock = r.clock.Add(linkLifetime + time.Second)
	_, out = r.call("POST", "/link/status", map[string]any{"link_id": out["link_id"]})
	if out["state"] != "failed" || !strings.Contains(out["error"].(string), "ran out") {
		t.Fatalf("after the code's lifetime: %v", out)
	}
	last := r.back.made[len(r.back.made)-1]
	if !last.forgotten {
		t.Fatal("the abandoned session's keys were kept")
	}
}

// ---- reading ---------------------------------------------------------------

func TestIncomingMessagesAreStoredCountedUnreadAndSearchable(t *testing.T) {
	r := newRig(t)
	r.incoming(alice, "IN2", "Bring the tickets")
	r.incoming(alice, "IN2", "Bring the tickets") // delivered twice
	_, out := r.call("GET", "/chats?account="+me+"&unread=1", nil)
	chats := out["chats"].([]any)
	c := chats[0].(map[string]any)
	if len(chats) != 1 || c["unread"].(float64) != 2 || c["name"] != "Alice Example" || c["last_message"] != "Bring the tickets" {
		t.Fatalf("chat list: %v", chats)
	}
	_, out = r.call("GET", "/messages?account="+me+"&chat=Alice+Example", nil)
	msgs := out["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["text"] != "Are we still on for Friday?" {
		t.Fatalf("messages oldest first: %v", msgs)
	}
	_, out = r.call("GET", "/search?account="+me+"&query=tickets", nil)
	hits := out["messages"].([]any)
	if len(hits) != 1 || hits[0].(map[string]any)["chat_name"] != "Alice Example" {
		t.Fatalf("search: %v", hits)
	}
	_, out = r.call("GET", "/contacts?account="+me+"&query=carol", nil)
	people := out["contacts"].([]any)
	if len(people) != 1 || people[0].(map[string]any)["has_chat"] != false || people[0].(map[string]any)["number"] != "+447700900333" {
		t.Fatalf("contacts: %v", people)
	}
}

func TestAHiddenIdIsFiledUnderThePersonsNumber(t *testing.T) {
	r := newRig(t)
	r.incoming("99001@lid", "IN9", "from my hidden id")
	_, out := r.call("GET", "/messages?account="+me+"&chat="+alice, nil)
	if n := len(out["messages"].([]any)); n != 2 {
		t.Fatalf("the hidden-id message was filed elsewhere (%d in Alice's chat)", n)
	}
}

func TestHistoryIsStoredWithoutInventingUnreadMessages(t *testing.T) {
	r := newRig(t)
	bob := "447700900777@s.whatsapp.net"
	web := func(id, text string, fromMe bool, ts uint64) *waHistorySync.HistorySyncMsg {
		return &waHistorySync.HistorySyncMsg{Message: &waWeb.WebMessageInfo{
			Key:              &waCommon.MessageKey{RemoteJID: proto.String(bob), ID: proto.String(id), FromMe: proto.Bool(fromMe)},
			MessageTimestamp: proto.Uint64(ts), Message: &waE2E.Message{Conversation: proto.String(text)}}}
	}
	r.client.emit(&events.HistorySync{Data: &waHistorySync.HistorySync{
		Conversations: []*waHistorySync.Conversation{
			{ID: proto.String(bob), Name: proto.String("Bob Builder"), UnreadCount: proto.Uint32(1),
				Messages: []*waHistorySync.HistorySyncMsg{web("H2", "newer", false, 2000), web("H1", "older", true, 1000)}},
			{ID: proto.String("status@broadcast"), Messages: []*waHistorySync.HistorySyncMsg{web("S1", "a status", false, 1)}},
		}}})
	_, out := r.call("GET", "/chats?account="+me+"&query=Bob", nil)
	chats := out["chats"].([]any)
	if len(chats) != 1 || chats[0].(map[string]any)["unread"].(float64) != 1 {
		t.Fatalf("history chat: %v", chats)
	}
	_, out = r.call("GET", "/messages?account="+me+"&chat="+bob, nil)
	msgs := out["messages"].([]any)
	if len(msgs) != 2 || msgs[0].(map[string]any)["text"] != "older" || msgs[0].(map[string]any)["from_me"] != true {
		t.Fatalf("history messages: %v", msgs)
	}
	if _, out = r.call("GET", "/search?account="+me+"&query=a+status", nil); len(out["messages"].([]any)) != 0 {
		t.Fatal("a status update was stored as a chat message")
	}
}

func TestViewOnceDeletedAndEditedMessages(t *testing.T) {
	r := newRig(t)
	a, _ := types.ParseJID(alice)
	src := types.MessageSource{Chat: a, Sender: a}
	r.client.emit(&events.Message{Info: types.MessageInfo{MessageSource: src, ID: "V1", Timestamp: r.clock}, IsViewOnce: true,
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), Caption: proto.String("secret")}}})
	if status, _ := r.call("POST", "/media", map[string]any{"account": me, "chat": alice, "id": "V1"}); status == 200 {
		t.Fatal("a view-once photo could be fetched")
	}
	_, out := r.call("GET", "/search?account="+me+"&query=secret", nil)
	if len(out["messages"].([]any)) != 0 {
		t.Fatal("a view-once caption was stored")
	}
	key := &waCommon.MessageKey{ID: proto.String("IN1")}
	r.client.emit(&events.Message{Info: types.MessageInfo{MessageSource: src, ID: "E1", Timestamp: r.clock},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: key, Type: waE2E.ProtocolMessage_MESSAGE_EDIT.Enum(),
			EditedMessage: &waE2E.Message{Conversation: proto.String("Are we still on for Saturday?")}}}})
	m, _ := r.m.store.Message(me, alice, "IN1")
	if m.Text != "Are we still on for Saturday?" {
		t.Fatalf("edit not applied: %q", m.Text)
	}
	r.client.emit(&events.Message{Info: types.MessageInfo{MessageSource: src, ID: "D1", Timestamp: r.clock},
		Message: &waE2E.Message{ProtocolMessage: &waE2E.ProtocolMessage{Key: key, Type: waE2E.ProtocolMessage_REVOKE.Enum()}}})
	m, _ = r.m.store.Message(me, alice, "IN1")
	if m.Kind != "deleted" || strings.Contains(m.Text, "Saturday") {
		t.Fatalf("a deleted message kept its text: %+v", m)
	}
}

func TestReadingDoesNotSendReadReceiptsButMarkReadDoes(t *testing.T) {
	r := newRig(t)
	r.call("GET", "/messages?account="+me+"&chat="+alice, nil)
	r.call("GET", "/chats?account="+me, nil)
	if len(r.client.read) != 0 {
		t.Fatal("reading a chat told WhatsApp it was read")
	}
	status, out := r.call("POST", "/read", map[string]any{"account": me, "chat": alice})
	if status != 200 || out["marked"].(float64) != 1 || len(r.client.read) != 1 || r.client.read[0][0] != "IN1" {
		t.Fatalf("mark read: %d %v %v", status, out, r.client.read)
	}
	_, out = r.call("GET", "/chats?account="+me+"&unread=1", nil)
	if len(out["chats"].([]any)) != 0 {
		t.Fatal("still unread after mark read")
	}
	// Read on the phone: no longer unread here either.
	r.incoming(alice, "IN3", "ping")
	a, _ := types.ParseJID(alice)
	r.client.emit(&events.Receipt{MessageSource: types.MessageSource{Chat: a}, Type: types.ReceiptTypeReadSelf})
	_, out = r.call("GET", "/chats?account="+me+"&unread=1", nil)
	if len(out["chats"].([]any)) != 0 {
		t.Fatal("a chat read on the phone still counts as unread")
	}
}

// ---- replies, files, media --------------------------------------------------

func TestAReplyQuotesTheMessageAndAFileIsUploadedAsItsKind(t *testing.T) {
	r := newRig(t)
	if status, msg := r.send(map[string]any{"to": alice, "text": "Yes!", "reply_to": "IN1"}); status != 200 {
		t.Fatal(msg)
	}
	ci := r.client.sent[0].msg.GetExtendedTextMessage().GetContextInfo()
	if ci.GetStanzaID() != "IN1" || ci.GetParticipant() != alice {
		t.Fatalf("reply context: %v", ci)
	}
	if status, _ := r.send(map[string]any{"to": alice, "text": "x", "reply_to": "NOPE"}); status == 200 {
		t.Fatal("a reply to a message that is not in the chat was sent")
	}
	png := filepath.Join(r.dir, "photo.png")
	pdf := filepath.Join(r.dir, "quote.pdf")
	os.WriteFile(png, []byte("\x89PNG\r\n\x1a\n"), 0o600)
	os.WriteFile(pdf, []byte("%PDF-1.4"), 0o600)
	r.clock = r.clock.Add(time.Minute)
	if status, msg := r.send(map[string]any{"to": alice, "file": png, "text": "The photo"}); status != 200 {
		t.Fatal(msg)
	}
	r.clock = r.clock.Add(time.Minute)
	if status, msg := r.send(map[string]any{"to": alice, "file": pdf}); status != 200 {
		t.Fatal(msg)
	}
	if img := r.client.sent[1].msg.GetImageMessage(); img.GetCaption() != "The photo" || img.GetMimetype() != "image/png" {
		t.Fatalf("image: %v", img)
	}
	if doc := r.client.sent[2].msg.GetDocumentMessage(); doc.GetFileName() != "quote.pdf" {
		t.Fatalf("document: %v", doc)
	}
	before := len(r.client.sent)
	if status, _ := r.send(map[string]any{"to": alice, "file": filepath.Join(r.dir, "missing.pdf")}); status == 200 || len(r.client.sent) != before {
		t.Fatal("a missing file was sent")
	}
}

func TestMediaIsFetchedOnlyOnRequestAndSaysWhenItIsGone(t *testing.T) {
	r := newRig(t)
	a, _ := types.ParseJID(alice)
	r.client.emit(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: a, Sender: a}, ID: "P1", Timestamp: r.clock},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg"), Caption: proto.String("The view")}}})
	if entries, _ := os.ReadDir(filepath.Join(r.dir, "media")); len(entries) != 0 {
		t.Fatal("a photo was downloaded before anyone asked for it")
	}
	status, out := r.call("POST", "/media", map[string]any{"account": me, "chat": alice, "id": "P1"})
	if status != 200 {
		t.Fatalf("media: %d %v", status, out)
	}
	data, _ := os.ReadFile(out["path"].(string))
	if string(data) != "JPEGDATA" || !strings.HasPrefix(out["path"].(string), r.dir) {
		t.Fatalf("saved %q at %v", data, out["path"])
	}
	r.client.emit(&events.Message{Info: types.MessageInfo{MessageSource: types.MessageSource{Chat: a, Sender: a}, ID: "P2", Timestamp: r.clock},
		Message: &waE2E.Message{ImageMessage: &waE2E.ImageMessage{Mimetype: proto.String("image/jpeg")}}})
	r.client.media = nil
	status, out = r.call("POST", "/media", map[string]any{"account": me, "chat": alice, "id": "P2"})
	if status == 200 || !strings.Contains(fmt.Sprint(out), "no longer has that file") {
		t.Fatalf("expired media: %d %v", status, out)
	}
}

// ---- account state ---------------------------------------------------------

func TestAnUnlinkedOrRestrictedNumberRefusesToSendAndSaysWhy(t *testing.T) {
	r := newRig(t)
	r.client.emit(&events.LoggedOut{})
	status, msg := r.send(map[string]any{"to": alice, "text": "hi"})
	if status != 503 || !strings.Contains(msg, "Link it again") {
		t.Fatalf("logged out: %d %s", status, msg)
	}
	// History stays readable when the link has gone.
	if status, _ := r.call("GET", "/messages?account="+me+"&chat="+alice, nil); status != 200 {
		t.Fatal("history became unreadable after the link expired")
	}
	_, out := r.call("GET", "/accounts", nil)
	if acct := out["accounts"].([]any)[0].(map[string]any); acct["usable"] != false || acct["state"] != "logged_out" {
		t.Fatalf("account state: %v", acct)
	}
	r.client.emit(&events.TemporaryBan{Code: events.TempBanSentToTooManyPeople, Expire: time.Hour})
	if _, msg = r.send(map[string]any{"to": alice, "text": "hi"}); !strings.HasPrefix(msg, "restricted:") {
		t.Fatalf("restricted: %s", msg)
	}
	r.client.emit(&events.ClientOutdated{})
	if _, msg = r.send(map[string]any{"to": alice, "text": "hi"}); !strings.Contains(msg, "Update Owlka") {
		t.Fatalf("outdated: %s", msg)
	}
	r.nothingSent("unusable account")
}

func TestAFailedSendIsReportedAndNotCounted(t *testing.T) {
	r := newRig(t)
	r.client.sendErr = whatsmeow.ErrNotConnected
	status, msg := r.send(map[string]any{"to": alice, "text": "hi"})
	if status == 200 || !strings.Contains(msg, "may not have been sent") {
		t.Fatalf("%d %s", status, msg)
	}
	if hist, _ := r.m.store.RecentSends(me, r.clock); len(hist) != 0 {
		t.Fatal("a failed send was counted")
	}
}

func TestASendTheStoreCouldNotRecordStillCountsTowardsTheLimits(t *testing.T) {
	r := newRig(t)
	r.m.limits.MinGap = 0
	r.m.sleep = func(context.Context, time.Duration) {}
	// The record of sends becomes unwritable: every send still goes, and
	// every one must still be counted.
	if _, err := r.m.store.db.Exec(`CREATE TRIGGER sends_unwritable BEFORE INSERT ON sends BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	sent := 0
	for i := 0; i < DefaultLimits.PerMinute+3; i++ {
		if status, _ := r.send(map[string]any{"to": alice, "text": fmt.Sprintf("message %d", i)}); status == 200 {
			sent++
		}
	}
	if sent != DefaultLimits.PerMinute || len(r.client.sent) != DefaultLimits.PerMinute {
		t.Fatalf("%d messages went out with the send record broken; the limit is %d", len(r.client.sent), DefaultLimits.PerMinute)
	}
}

func TestRemovingANumberUnlinksItAndDeletesItsHistory(t *testing.T) {
	r := newRig(t)
	os.MkdirAll(filepath.Join(r.dir, "media", me), 0o700)
	status, out := r.call("POST", "/accounts/remove", map[string]any{"account": me})
	if status != 200 || !r.client.loggedOut {
		t.Fatalf("remove: %d %v", status, out)
	}
	if m, _ := r.m.store.Message(me, alice, "IN1"); m != nil {
		t.Fatal("messages were kept after removal")
	}
	if _, err := os.Stat(filepath.Join(r.dir, "media", me)); !os.IsNotExist(err) {
		t.Fatal("downloaded files were kept after removal")
	}
	if status, _ := r.call("GET", "/chats?account="+me, nil); status != 404 {
		t.Fatal("a removed number still answers")
	}
}

// ---- the local socket ------------------------------------------------------

func TestEveryRouteNeedsTheTokenAndRefusesABrowser(t *testing.T) {
	r := newRig(t)
	for _, path := range []string{"/health", "/accounts", "/chats?account=" + me} {
		resp, _ := http.Get(r.srv.URL + path)
		if resp.StatusCode != 401 {
			t.Fatalf("%s answered without the token (%d)", path, resp.StatusCode)
		}
	}
	req, _ := http.NewRequest("POST", r.srv.URL+"/send", strings.NewReader(`{"account":"`+me+`","to":"`+alice+`","text":"hi"}`))
	req.Header.Set("Authorization", "Bearer TOKEN")
	req.Header.Set("Origin", "https://evil.example")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != 401 {
		t.Fatalf("a request from a web page was accepted (%d)", resp.StatusCode)
	}
	r.nothingSent("unauthorised callers")
}

func TestTwoNumbersKeepSeparateHistoryAndSeparateLimits(t *testing.T) {
	r := newRig(t)
	_, out := r.call("POST", "/link/start", map[string]any{"phone": "+447700900555"})
	_ = out
	second := r.back.made[0]
	second.phoneEntersCode("447700900555")
	if status, out := r.call("GET", "/chats?account=447700900555", nil); status != 200 || strings.Contains(fmt.Sprint(out["chats"]), "447700900111") {
		t.Fatalf("the second number can see the first number's chats: %v", out)
	}
	status, body := r.call("POST", "/send", map[string]any{"account": "447700900555", "to": alice, "text": "hi"})
	if status != 403 || len(second.sent) != 0 {
		t.Fatalf("the second number wrote to the first number's chat: %d %v", status, body)
	}
}

func TestMain(m *testing.M) {
	log.SetOutput(io.Discard) // background name refreshes outlive a test's store
	os.Exit(m.Run())
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("timed out waiting")
}
