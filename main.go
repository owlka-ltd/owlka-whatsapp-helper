// owlka-whatsapp-helper. Copyright (C) 2026 Owlka Ltd. GPL-3.0, see LICENSE.

// Command owlka-whatsapp is a small local program that links to WhatsApp as a
// linked device (the same mechanism as WhatsApp Web) and offers what it can do
// over a socket on this computer only. See README.md and PROTOCOL.md.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

// StateFile is how a caller finds the helper: its port and the token it
// expects. Written with owner-only permissions.
type StateFile struct {
	Port    int    `json:"port"`
	Token   string `json:"token"`
	PID     int    `json:"pid"`
	Version string `json:"version"`
}

const stateName = "helper.json"

// alreadyRunning reports whether another helper is serving this folder.
func alreadyRunning(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, stateName))
	if err != nil {
		return false
	}
	var st StateFile
	if json.Unmarshal(raw, &st) != nil || st.Port == 0 {
		return false
	}
	req, _ := http.NewRequest("GET", fmt.Sprintf("http://127.0.0.1:%d/health", st.Port), nil)
	req.Header.Set("Authorization", "Bearer "+st.Token)
	resp, err := (&http.Client{Timeout: 3 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

func main() {
	dir := flag.String("dir", "", "folder for this user's WhatsApp session and message history (required)")
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(Version)
		return
	}
	if *dir == "" {
		fmt.Fprintln(os.Stderr, "owlka-whatsapp: --dir is required")
		os.Exit(2)
	}
	if err := run(*dir); err != nil {
		fmt.Fprintln(os.Stderr, "owlka-whatsapp:", err)
		os.Exit(1)
	}
}

func run(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	// Session keys and messages: this user only.
	_ = os.Chmod(dir, 0o700)
	for _, name := range []string{"messages.db", "session.db"} {
		if f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_RDWR, 0o600); err == nil {
			f.Close()
		}
	}
	if alreadyRunning(dir) {
		return nil // one helper per folder; the running one keeps the session
	}
	if logFile, err := os.OpenFile(filepath.Join(dir, "helper.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600); err == nil {
		log.SetOutput(logFile)
		defer logFile.Close()
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	store, err := OpenStore(filepath.Join(dir, "messages.db"))
	if err != nil {
		return err
	}
	defer store.Close()
	backend, err := openMeow(ctx, filepath.Join(dir, "session.db"))
	if err != nil {
		return fmt.Errorf("open session store: %w", err)
	}
	manager := NewManager(backend, store, dir)
	if err := manager.Start(ctx); err != nil {
		return fmt.Errorf("start sessions: %w", err)
	}
	defer manager.Stop()

	// This computer only, on a port the system picks.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	tok := make([]byte, 32)
	if _, err := rand.Read(tok); err != nil {
		return err
	}
	state := StateFile{Port: ln.Addr().(*net.TCPAddr).Port, Token: hex.EncodeToString(tok), PID: os.Getpid(), Version: Version}
	raw, _ := json.Marshal(state)
	statePath := filepath.Join(dir, stateName)
	tmp := statePath + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, statePath); err != nil {
		return err
	}
	api := &API{m: manager, token: state.Token, stop: cancel}
	srv := &http.Server{Handler: api.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shut, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shut)
	}()
	log.Printf("helper %s listening on 127.0.0.1:%d", Version, state.Port)
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}
