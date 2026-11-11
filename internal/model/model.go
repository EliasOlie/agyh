// Package model holds the agent-agnostic types shared by every part of agyh.
package model

import (
	"strings"
	"time"
)

// Session is one conversation with a coding agent, normalized across agents.
type Session struct {
	Agent     string    `json:"agent"`
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Preview   string    `json:"preview,omitempty"`
	Workspace string    `json:"workspace,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  int       `json:"messages"`
	Status    string    `json:"status,omitempty"`
	Parent    string    `json:"parent,omitempty"`
	// Path is the file the session was loaded from; ModTime drives incremental indexing.
	Path    string    `json:"-"`
	ModTime time.Time `json:"-"`
}

// Key is the globally unique identifier of a session ("agent:id").
func (s Session) Key() string { return s.Agent + ":" + s.ID }

// ShortID is the first 8 characters of the ID, enough to address a session.
func (s Session) ShortID() string {
	if len(s.ID) > 8 {
		return s.ID[:8]
	}
	return s.ID
}

// Message is one human-readable turn. Tool calls and system noise are dropped.
type Message struct {
	Role string    `json:"role"` // "user" or "assistant"
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// Truncate shortens s to at most n runes, collapsing whitespace, adding "…" when cut.
func Truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return "…"
	}
	return string(r[:n-1]) + "…"
}
