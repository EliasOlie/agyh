// Package hook translates between each agent's hook JSON and agyh's
// agent-neutral approval request. Claude Code and Antigravity both run a
// command before a tool call and read a decision from its stdout, with
// different field names.
package hook

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/EliasOlie/agyh/internal/policy"
)

// Request is a tool call waiting for permission, or a finished turn (Stop).
type Request struct {
	Agent          string `json:"agent"`
	SessionID      string `json:"session_id"`
	Tool           string `json:"tool"`
	Command        string `json:"command"` // shell command, when the tool runs one
	Summary        string `json:"summary"` // human description for non-shell tools
	Workspace      string `json:"workspace"`
	TranscriptPath string `json:"transcript_path"`
}

// IsShell reports whether the request runs a shell command (policy applies).
func (r Request) IsShell() bool { return r.Command != "" }

// Describe is what the phone shows.
func (r Request) Describe() string {
	if r.Command != "" {
		return r.Command
	}
	return firstNonEmpty(r.Summary, r.Tool)
}

type claudeInput struct {
	SessionID      string         `json:"session_id"`
	TranscriptPath string         `json:"transcript_path"`
	Cwd            string         `json:"cwd"`
	ToolName       string         `json:"tool_name"`
	ToolInput      map[string]any `json:"tool_input"`
}

type agyInput struct {
	ConversationID string   `json:"conversationId"`
	WorkspacePaths []string `json:"workspacePaths"`
	TranscriptPath string   `json:"transcriptPath"`
	ToolCall       struct {
		Name string         `json:"name"`
		Args map[string]any `json:"args"`
	} `json:"toolCall"`
}

// Parse reads a hook payload from stdin for the given agent.
func Parse(agent string, r io.Reader) (Request, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 4<<20))
	if err != nil {
		return Request{}, err
	}
	switch agent {
	case "claude":
		var in claudeInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return Request{}, fmt.Errorf("parse claude hook input: %w", err)
		}
		req := Request{Agent: agent, SessionID: in.SessionID, Tool: in.ToolName,
			Workspace: in.Cwd, TranscriptPath: in.TranscriptPath}
		if in.ToolName == "Bash" {
			req.Command = str(in.ToolInput["command"])
		} else {
			req.Summary = summarize(in.ToolName, in.ToolInput)
		}
		return req, nil
	case "agy":
		var in agyInput
		if err := json.Unmarshal(raw, &in); err != nil {
			return Request{}, fmt.Errorf("parse agy hook input: %w", err)
		}
		req := Request{Agent: agent, SessionID: in.ConversationID, Tool: firstNonEmpty(in.ToolCall.Name, "run_command"),
			TranscriptPath: in.TranscriptPath}
		if len(in.WorkspacePaths) > 0 {
			req.Workspace = in.WorkspacePaths[0]
		}
		if cmd := str(in.ToolCall.Args["CommandLine"]); cmd != "" {
			req.Command = cmd
		} else {
			req.Summary = summarize(req.Tool, in.ToolCall.Args)
		}
		return req, nil
	}
	return Request{}, fmt.Errorf("unsupported agent %q (use claude or agy)", agent)
}

// Output renders a decision in the agent's expected stdout format.
// For Claude Code, Ask hands control back to its own permission prompt.
// Antigravity only understands allow/deny, so Ask becomes deny.
func Output(agent string, v policy.Verdict, reason string) []byte {
	var out any
	switch agent {
	case "claude":
		out = map[string]any{"hookSpecificOutput": map[string]any{
			"hookEventName":            "PreToolUse",
			"permissionDecision":       v.String(),
			"permissionDecisionReason": reason,
		}}
	default:
		d := v.String()
		if v == policy.Ask {
			d = "deny"
		}
		out = map[string]any{"decision": d, "reason": reason}
	}
	b, _ := json.Marshal(out)
	return b
}

func summarize(tool string, in map[string]any) string {
	for _, k := range []string{"file_path", "path", "url", "pattern", "query", "TargetFile", "Url"} {
		if v := str(in[k]); v != "" {
			return tool + " " + v
		}
	}
	return tool
}

func str(v any) string {
	s, _ := v.(string)
	return strings.TrimSpace(s)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// ShortenHome replaces the home directory prefix with "~" for display.
func ShortenHome(p string) string {
	home, err := os.UserHomeDir()
	if err == nil && home != "" && strings.HasPrefix(p, home) {
		return "~" + p[len(home):]
	}
	return p
}
