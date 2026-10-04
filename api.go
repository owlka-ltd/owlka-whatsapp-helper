// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Version is the protocol and program version. PROTOCOL.md describes every
// route below.
const Version = "0.1.0"

type API struct {
	m     *Manager
	token string
	stop  func()
}

func (a *API) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", a.health)
	mux.HandleFunc("POST /shutdown", a.shutdown)
	mux.HandleFunc("GET /accounts", a.accounts)
	mux.HandleFunc("POST /accounts/remove", a.remove)
	mux.HandleFunc("POST /link/start", a.linkStart)
	mux.HandleFunc("POST /link/status", a.linkStatus)
	mux.HandleFunc("GET /chats", a.chats)
	mux.HandleFunc("GET /messages", a.messages)
	mux.HandleFunc("GET /search", a.search)
	mux.HandleFunc("GET /contacts", a.contacts)
	mux.HandleFunc("POST /send", a.send)
	mux.HandleFunc("POST /read", a.read)
	mux.HandleFunc("POST /media", a.media)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only a program that can read this user's state file may talk to
		// the helper, and only from this computer. A browser page cannot.
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if subtle.ConstantTimeCompare([]byte(got), []byte(a.token)) != 1 || r.Header.Get("Origin") != "" {
			fail(w, http.StatusUnauthorized, &Refusal{"unauthorized", "This request did not come from Owlka."})
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, status int, r *Refusal) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": r.Code, "message": r.Message}})
}

// refused answers with the status that fits the refusal.
func refused(w http.ResponseWriter, r *Refusal) {
	status := http.StatusBadRequest
	switch r.Code {
	case "rate_limited", "broadcast", "new_number_limit":
		status = http.StatusTooManyRequests
	case "no_account", "no_chat", "no_message", "no_link":
		status = http.StatusNotFound
	case "unknown_number", "unknown_chat":
		status = http.StatusForbidden
	case "offline", "logged_out", "restricted", "outdated":
		status = http.StatusServiceUnavailable
	case "store", "send_failed", "upload_failed":
		status = http.StatusBadGateway
	}
	fail(w, status, r)
}

// body decodes a JSON request strictly: an unknown field, or the wrong type
// (for example a LIST of recipients), is an error and nothing happens.
func body(w http.ResponseWriter, r *http.Request, into any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		fail(w, http.StatusBadRequest, refuse("bad_request", "The request was not understood: %v", err))
		return false
	}
	return true
}

func limitOf(r *http.Request, def, max int) int {
	n, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || n <= 0 {
		return def
	}
	return min(n, max)
}

func (a *API) health(w http.ResponseWriter, _ *http.Request) {
	reply(w, map[string]any{"ok": true, "version": Version, "accounts": a.m.Accounts()})
}

func (a *API) shutdown(w http.ResponseWriter, _ *http.Request) {
	reply(w, map[string]any{"ok": true})
	go a.stop()
}

func (a *API) accounts(w http.ResponseWriter, _ *http.Request) {
	reply(w, map[string]any{"accounts": a.m.Accounts()})
}

func (a *API) remove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string `json:"account"`
	}
	if !body(w, r, &req) {
		return
	}
	note, ref := a.m.Remove(r.Context(), req.Account)
	if ref != nil {
		refused(w, ref)
		return
	}
	reply(w, map[string]any{"ok": true, "detail": note})
}

func (a *API) linkStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Phone string `json:"phone"`
	}
	if !body(w, r, &req) {
		return
	}
	l, ref := a.m.StartLink(r.Context(), req.Phone)
	if ref != nil {
		refused(w, ref)
		return
	}
	s, ref := a.m.LinkStatus(l.id)
	if ref != nil {
		refused(w, ref)
		return
	}
	reply(w, s)
}

func (a *API) linkStatus(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LinkID string `json:"link_id"`
	}
	if !body(w, r, &req) {
		return
	}
	s, ref := a.m.LinkStatus(req.LinkID)
	if ref != nil {
		refused(w, ref)
		return
	}
	reply(w, s)
}

func (a *API) chats(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	acct, ref := a.m.account(q.Get("account"))
	if ref != nil {
		refused(w, ref)
		return
	}
	out, err := a.m.store.Chats(acct.id, q.Get("query"), q.Get("unread") == "1", limitOf(r, 30, 200))
	if err != nil {
		refused(w, refuse("store", "The local message store could not be read: %v", err))
		return
	}
	reply(w, map[string]any{"chats": orEmpty(out), "state": acct.info()})
}

func (a *API) messages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	acct, ref := a.m.account(q.Get("account"))
	if ref != nil {
		refused(w, ref)
		return
	}
	chat, ref := a.m.ResolveChat(acct, q.Get("chat"))
	if ref != nil {
		refused(w, ref)
		return
	}
	var before time.Time
	if b := q.Get("before"); b != "" {
		t, err := time.Parse(time.RFC3339, b)
		if err != nil {
			refused(w, refuse("bad_request", "before must be a time like 2026-10-04T12:00:00Z."))
			return
		}
		before = t
	}
	out, err := a.m.store.Messages(acct.id, chat, limitOf(r, 30, 200), before)
	if err != nil {
		refused(w, refuse("store", "The local message store could not be read: %v", err))
		return
	}
	reply(w, map[string]any{"chat": chat, "messages": orEmpty(out), "state": acct.info()})
}

func (a *API) search(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	acct, ref := a.m.account(q.Get("account"))
	if ref != nil {
		refused(w, ref)
		return
	}
	if strings.TrimSpace(q.Get("query")) == "" {
		refused(w, refuse("bad_request", "Say what to search for."))
		return
	}
	chat := ""
	if q.Get("chat") != "" {
		if chat, ref = a.m.ResolveChat(acct, q.Get("chat")); ref != nil {
			refused(w, ref)
			return
		}
	}
	out, err := a.m.store.Search(acct.id, q.Get("query"), chat, limitOf(r, 30, 200))
	if err != nil {
		refused(w, refuse("store", "The local message store could not be read: %v", err))
		return
	}
	reply(w, map[string]any{"messages": orEmpty(out), "state": acct.info()})
}

func (a *API) contacts(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	acct, ref := a.m.account(q.Get("account"))
	if ref != nil {
		refused(w, ref)
		return
	}
	out, err := a.m.store.People(acct.id, q.Get("query"), false, limitOf(r, 20, 100))
	if err != nil {
		refused(w, refuse("store", "The local message store could not be read: %v", err))
		return
	}
	reply(w, map[string]any{"contacts": orEmpty(out)})
}

func (a *API) send(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	if !body(w, r, &req) {
		return
	}
	res, ref := a.m.Send(r.Context(), req)
	if ref != nil {
		refused(w, ref)
		return
	}
	reply(w, res)
}

func (a *API) read(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string `json:"account"`
		Chat    string `json:"chat"`
	}
	if !body(w, r, &req) {
		return
	}
	n, chat, ref := a.m.MarkRead(r.Context(), req.Account, req.Chat)
	if ref != nil {
		refused(w, ref)
		return
	}
	reply(w, map[string]any{"chat": chat, "marked": n})
}

func (a *API) media(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Account string `json:"account"`
		Chat    string `json:"chat"`
		ID      string `json:"id"`
	}
	if !body(w, r, &req) {
		return
	}
	path, msg, ref := a.m.Media(r.Context(), req.Account, req.Chat, req.ID)
	if ref != nil {
		refused(w, ref)
		return
	}
	reply(w, map[string]any{"path": path, "kind": msg.Kind, "media_type": msg.MediaMime, "file_name": msg.MediaName})
}

func orEmpty[T any](s []T) []T {
	if s == nil {
		return []T{}
	}
	return s
}
