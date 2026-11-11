package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"net/http"
	"strings"
	"time"

	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/policy"
)

// TelegramClient is a minimal Bot API client (no third-party SDK).
type TelegramClient struct {
	Token   string
	BaseURL string // defaults to https://api.telegram.org; overridden in tests
	HTTP    *http.Client
}

func NewTelegramClient(token string) *TelegramClient {
	return &TelegramClient{Token: token, BaseURL: "https://api.telegram.org", HTTP: &http.Client{Timeout: 70 * time.Second}}
}

type tgResponse struct {
	OK          bool            `json:"ok"`
	Result      json.RawMessage `json:"result"`
	Description string          `json:"description"`
}

// Call invokes a Bot API method. The token never appears in returned errors.
func (c *TelegramClient) Call(ctx context.Context, method string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+c.Token+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var uerr interface{ Unwrap() error }
		if errors.As(err, &uerr) && ctx.Err() == nil {
			return fmt.Errorf("telegram %s: %w", method, uerr.Unwrap()) // drop the URL (contains the token)
		}
		return fmt.Errorf("telegram %s: request failed", method)
	}
	defer resp.Body.Close()
	var r tgResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: bad response (HTTP %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

type TgUser struct {
	ID       int64  `json:"id"`
	Username string `json:"username"`
}

type tgChat struct {
	ID int64 `json:"id"`
}

type tgMessage struct {
	MessageID int64  `json:"message_id"`
	Chat      tgChat `json:"chat"`
	Text      string `json:"text"`
	From      TgUser `json:"from"`
}

type tgCallback struct {
	ID      string     `json:"id"`
	Data    string     `json:"data"`
	Message *tgMessage `json:"message"`
}

type TgUpdate struct {
	UpdateID int64       `json:"update_id"`
	Message  *tgMessage  `json:"message"`
	Callback *tgCallback `json:"callback_query"`
}

func (c *TelegramClient) GetMe(ctx context.Context) (TgUser, error) {
	var u TgUser
	return u, c.Call(ctx, "getMe", map[string]any{}, &u)
}

func (c *TelegramClient) GetUpdates(ctx context.Context, offset int64, timeoutSec int) ([]TgUpdate, error) {
	var ups []TgUpdate
	err := c.Call(ctx, "getUpdates", map[string]any{
		"offset": offset, "timeout": timeoutSec, "allowed_updates": []string{"message", "callback_query"},
	}, &ups)
	return ups, err
}

// FirstChat waits for someone to message the bot and returns their chat id (used during onboarding).
func (c *TelegramClient) FirstChat(ctx context.Context) (chatID int64, from TgUser, err error) {
	var offset int64
	for {
		ups, err := c.GetUpdates(ctx, offset, 25)
		if err != nil {
			return 0, TgUser{}, err
		}
		for _, u := range ups {
			offset = u.UpdateID + 1
			if u.Message != nil {
				// Acknowledge so the bridge does not replay this update later.
				_, _ = c.GetUpdates(ctx, offset, 0)
				return u.Message.Chat.ID, u.Message.From, nil
			}
		}
	}
}

// Telegram is the Telegram channel. It only talks to one chat id: messages
// from anyone else are ignored, so a leaked bot username grants nothing.
type Telegram struct {
	client *TelegramClient
	chatID int64
}

func NewTelegram(client *TelegramClient, chatID int64) *Telegram {
	return &Telegram{client: client, chatID: chatID}
}

func (t *Telegram) Name() string   { return "telegram" }
func (t *Telegram) Markup() Markup { return tgMarkup{} }

type tgMarkup struct{}

func (tgMarkup) Bold(s string) string   { return "<b>" + s + "</b>" }
func (tgMarkup) Code(s string) string   { return "<code>" + html.EscapeString(s) + "</code>" }
func (tgMarkup) Escape(s string) string { return html.EscapeString(s) }

func (t *Telegram) Run(ctx context.Context, b *Bridge) error {
	cmds := make([]map[string]string, 0, len(Commands))
	for _, c := range Commands {
		cmds = append(cmds, map[string]string{"command": c.Name, "description": c.Help})
	}
	if err := t.client.Call(ctx, "setMyCommands", map[string]any{"commands": cmds}, nil); err != nil {
		b.log.Warn("telegram setMyCommands failed", "err", err)
	}
	var offset int64
	backoff := time.Second
	for {
		ups, err := t.client.GetUpdates(ctx, offset, 50)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			b.log.Warn("telegram poll failed", "err", err, "retry_in", backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return ctx.Err()
			}
			backoff = min(backoff*2, time.Minute)
			continue
		}
		backoff = time.Second
		for _, u := range ups {
			offset = u.UpdateID + 1
			t.handle(ctx, b, u)
		}
	}
}

func (t *Telegram) handle(ctx context.Context, b *Bridge, u TgUpdate) {
	switch {
	case u.Message != nil:
		if u.Message.Chat.ID != t.chatID {
			b.log.Warn("telegram message from unknown chat ignored", "chat", u.Message.Chat.ID)
			return
		}
		b.HandleText(ctx, t, u.Message.Text)
	case u.Callback != nil:
		cb := u.Callback
		if cb.Message == nil || cb.Message.Chat.ID != t.chatID {
			return
		}
		id, action, ok := parseCallback(cb.Data)
		text := ""
		if ok {
			v, auto := policy.Deny, false
			switch action {
			case "allow":
				v = policy.Allow
			case "auto":
				v, auto = policy.Allow, true
			}
			d := b.Decide(ctx, id, Decision{Verdict: v, Reason: "answered on telegram", By: "telegram"}, auto)
			if d.By == "bridge" {
				text = "Already decided or expired"
			}
		}
		_ = t.client.Call(ctx, "answerCallbackQuery", map[string]any{"callback_query_id": cb.ID, "text": text}, nil)
	}
}

// Callback data: "ap:<id>:<allow|deny|auto>".
func parseCallback(data string) (id, action string, ok bool) {
	parts := strings.Split(data, ":")
	if len(parts) != 3 || parts[0] != "ap" {
		return "", "", false
	}
	switch parts[2] {
	case "allow", "deny", "auto":
		return parts[1], parts[2], true
	}
	return "", "", false
}

// Send splits long text into Telegram-sized messages.
func (t *Telegram) Send(ctx context.Context, text string) error {
	for _, part := range chunk(text, 3900) {
		if err := t.client.Call(ctx, "sendMessage", map[string]any{
			"chat_id": t.chatID, "text": part, "parse_mode": "HTML", "disable_web_page_preview": true,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func approvalText(m Markup, a *Approval) string {
	return fmt.Sprintf("🔐 %s wants to run (%s)\n\n%s\n\n📁 %s · id %s",
		m.Bold(a.Req.Agent), m.Escape(a.Req.Tool), m.Code(model.Truncate(a.Req.Describe(), 900)),
		m.Escape(shortWS(a.Req.Workspace)), m.Code(a.ID))
}

func (t *Telegram) PromptApproval(ctx context.Context, a *Approval) error {
	kb := map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✅ Allow", "callback_data": "ap:" + a.ID + ":allow"},
		{"text": "❌ Deny", "callback_data": "ap:" + a.ID + ":deny"},
	}, {
		{"text": "⚡ Allow whole session", "callback_data": "ap:" + a.ID + ":auto"},
	}}}
	var msg tgMessage
	err := t.client.Call(ctx, "sendMessage", map[string]any{
		"chat_id": t.chatID, "text": approvalText(t.Markup(), a), "parse_mode": "HTML", "reply_markup": kb,
	}, &msg)
	if err == nil {
		a.SetHandle(t.Name(), msg.MessageID)
	}
	return err
}

func (t *Telegram) ApprovalResolved(ctx context.Context, a *Approval, d Decision) error {
	id, ok := a.Handle(t.Name()).(int64)
	if !ok {
		return nil
	}
	return t.client.Call(ctx, "editMessageText", map[string]any{
		"chat_id": t.chatID, "message_id": id, "parse_mode": "HTML",
		"text": approvalText(t.Markup(), a) + "\n\n" + outcome(d),
	}, nil)
}

func outcome(d Decision) string {
	switch d.Verdict {
	case policy.Allow:
		if d.By == "auto" {
			return "⚡ Allowed (autonomous)"
		}
		return "✅ Allowed via " + d.By
	case policy.Deny:
		return "❌ Denied via " + d.By
	}
	if d.By == "timeout" {
		return "⏱ No answer: back to the terminal prompt"
	}
	return "↩️ " + d.Reason
}

// chunk splits on line boundaries where possible.
func chunk(s string, n int) []string {
	var out []string
	for len(s) > n {
		cut := strings.LastIndex(s[:n], "\n")
		if cut < n/2 {
			cut = n
			for cut > 0 && !isRuneStart(s[cut]) {
				cut--
			}
		}
		out = append(out, s[:cut])
		s = strings.TrimLeft(s[cut:], "\n")
	}
	if s != "" {
		out = append(out, s)
	}
	return out
}

func isRuneStart(b byte) bool { return b&0xC0 != 0x80 }
