// owlka-whatsapp-helper: a local helper that links to WhatsApp as a linked device.
// Copyright (C) 2026 Owlka Ltd
//
// This program is free software: you can redistribute it and/or modify it under
// the terms of the GNU General Public License as published by the Free Software
// Foundation, version 3. It is distributed WITHOUT ANY WARRANTY. See LICENSE.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

// The send rules. They exist for one reason: a number that behaves like a bulk
// sender gets restricted by WhatsApp, and it is the customer's own number.
//
// THREAT MODEL: accidents. An assistant asked to "tell everyone" must not be
// able to turn one request into a mass send, and must not message a stranger by
// guessing a number. These rules are not a defence against someone who edits
// this program; the source is published.
//
// The rules are enforced HERE, in the one process that can send, so they hold
// whatever is calling and however many callers there are at once.

// Limits are per linked number.
type Limits struct {
	PerMinute            int
	PerHour              int
	PerDay               int
	NewNumbersPerDay     int           // first-ever messages to numbers with no chat
	SameTextChatsPerHour int           // one text to many chats is a broadcast
	MinGap               time.Duration // between two sends
}

var DefaultLimits = Limits{
	PerMinute:            8,
	PerHour:              60,
	PerDay:               250,
	NewNumbersPerDay:     3,
	SameTextChatsPerHour: 3,
	MinGap:               3 * time.Second,
}

// SendRecord is one send that happened.
type SendRecord struct {
	At        time.Time
	Chat      string
	TextHash  string
	NewNumber bool
}

// Refusal is a send the helper will not make. Message is a sentence for the
// person; Code is for programs.
type Refusal struct {
	Code    string
	Message string
}

func (r *Refusal) Error() string { return r.Message }

func refuse(code, format string, a ...any) *Refusal {
	return &Refusal{Code: code, Message: fmt.Sprintf(format, a...)}
}

// TextHash identifies a message body for the same-text rule. Whitespace and
// case are ignored so trivial edits do not make a broadcast look varied.
func TextHash(text string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
	if norm == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:8])
}

// CheckSend decides whether one more send is allowed now, given the sends
// already made from this number in the last day. It never queues: a refused
// send is reported, so nothing goes out later without anyone watching.
func CheckSend(history []SendRecord, now time.Time, next SendRecord, l Limits) *Refusal {
	var minute, hour, day, newToday int
	sameText := map[string]bool{}
	for _, s := range history {
		age := now.Sub(s.At)
		if age < 0 || age > 24*time.Hour {
			continue
		}
		day++
		if s.NewNumber {
			newToday++
		}
		if age <= time.Hour {
			hour++
			if next.TextHash != "" && s.TextHash == next.TextHash && s.Chat != next.Chat {
				sameText[s.Chat] = true
			}
		}
		if age <= time.Minute {
			minute++
		}
	}
	switch {
	case len(sameText) >= l.SameTextChatsPerHour:
		return refuse("broadcast",
			"This same message has already gone to %d other chats in the last hour. "+
				"Sending one message to many people is what gets a WhatsApp number restricted, so it was not sent.",
			len(sameText))
	case next.NewNumber && newToday >= l.NewNumbersPerDay:
		return refuse("new_number_limit",
			"%d numbers with no earlier chat have already been messaged today, which is the limit. "+
				"Messaging many new numbers is what gets a WhatsApp number restricted, so it was not sent. Try again tomorrow.",
			newToday)
	case minute >= l.PerMinute:
		return refuse("rate_limited",
			"%d messages have gone out in the last minute, which is the limit. Wait a minute and send again.", minute)
	case hour >= l.PerHour:
		return refuse("rate_limited",
			"%d messages have gone out in the last hour, which is the limit. Try again later.", hour)
	case day >= l.PerDay:
		return refuse("rate_limited",
			"%d messages have gone out in the last day, which is the limit. Try again tomorrow.", day)
	}
	return nil
}

// TypingDelay is how long the helper shows "typing" before a message goes, so
// it arrives the way a typed message does. jitter is in [0,1).
func TypingDelay(textLen int, jitter float64) time.Duration {
	base := 1200*time.Millisecond + time.Duration(textLen)*45*time.Millisecond
	if base > 9*time.Second {
		base = 9 * time.Second
	}
	// plus or minus 25%
	return time.Duration(float64(base) * (0.75 + 0.5*jitter))
}

// GapWait is how long to hold a send so that two sends are never closer than
// MinGap.
func GapWait(last, now time.Time, l Limits) time.Duration {
	if last.IsZero() {
		return 0
	}
	if wait := l.MinGap - now.Sub(last); wait > 0 {
		return wait
	}
	return 0
}

// digits keeps only the digits of a phone number.
func digits(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// looksLikeNumber is true when the text is a phone number rather than a name.
func looksLikeNumber(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !strings.ContainsRune("+ -().", r) {
			return false
		}
	}
	return len(digits(s)) >= 7
}
