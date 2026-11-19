package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/EliasOlie/agyh/internal/chatfmt"
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
	// Files sent from the phone.
	Caption  string   `json:"caption"`
	Document *tgFile  `json:"document"`
	Photo    []tgFile `json:"photo"` // sizes, smallest first
	Video    *tgFile  `json:"video"`
	Audio    *tgFile  `json:"audio"`
	Voice    *tgFile  `json:"voice"`
}

type tgFile struct {
	FileID   string `json:"file_id"`
	FileName string `json:"file_name"`
	MimeType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
}

// media is the file attached to a message, if any (photos: the largest size).
func (m *tgMessage) media() *tgFile {
	switch {
	case m.Document != nil:
		return m.Document
	case len(m.Photo) > 0:
		p := m.Photo[len(m.Photo)-1]
		p.MimeType = "image/jpeg"
		return &p
	case m.Video != nil:
		return m.Video
	case m.Audio != nil:
		return m.Audio
	case m.Voice != nil:
		return m.Voice
	}
	return nil
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

// tgDownloadLimit: bots can't download larger files from Telegram.
const tgDownloadLimit = 20 << 20

// Download fetches a file sent to the bot.
func (c *TelegramClient) Download(ctx context.Context, fileID string) (io.ReadCloser, error) {
	var f struct {
		FilePath string `json:"file_path"`
	}
	if err := c.Call(ctx, "getFile", map[string]any{"file_id": fileID}, &f); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/file/bot"+c.Token+"/"+f.FilePath, nil)
	if err != nil {
		return nil, errors.New("telegram download: bad file path")
	}
	resp, err := c.transfers().Do(req)
	if err != nil {
		return nil, errors.New("telegram download: request failed") // the URL holds the token
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("telegram download: HTTP %d", resp.StatusCode)
	}
	return resp.Body, nil
}

// Upload sends a file with a multipart request, streaming it from disk.
func (c *TelegramClient) Upload(ctx context.Context, method string, fields map[string]string, field, name, path string) error {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	go func() {
		err := func() error {
			for k, v := range fields {
				if err := mw.WriteField(k, v); err != nil {
					return err
				}
			}
			part, err := mw.CreateFormFile(field, name)
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			defer f.Close()
			if _, err := io.Copy(part, f); err != nil {
				return err
			}
			return mw.Close()
		}()
		pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/bot"+c.Token+"/"+method, pr)
	if err != nil {
		pr.Close()
		return fmt.Errorf("telegram %s: bad request", method)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.transfers().Do(req)
	if err != nil {
		pr.Close()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("telegram %s: upload failed", method) // the URL holds the token
	}
	defer resp.Body.Close()
	var r tgResponse
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: bad response (HTTP %d)", method, resp.StatusCode)
	}
	if !r.OK {
		return fmt.Errorf("telegram %s: %s", method, r.Description)
	}
	return nil
}

// transfers is an HTTP client for big files: no overall timeout (the
// caller's context bounds it), unlike the 70 s one for API calls.
func (c *TelegramClient) transfers() *http.Client {
	if c.HTTP != nil && c.HTTP.Timeout == 0 {
		return c.HTTP
	}
	tr := http.DefaultTransport
	if c.HTTP != nil && c.HTTP.Transport != nil {
		tr = c.HTTP.Transport
	}
	return &http.Client{Transport: tr}
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
			if strings.Contains(err.Error(), "Conflict") {
				select {
				case <-ctx.Done():
					return 0, TgUser{}, ctx.Err()
				case <-time.After(3 * time.Second):
					continue
				}
			}
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

func (tgMarkup) Bold(s string) string      { return "<b>" + s + "</b>" }
func (tgMarkup) Code(s string) string      { return "<code>" + html.EscapeString(s) + "</code>" }
func (tgMarkup) Escape(s string) string    { return html.EscapeString(s) }
func (tgMarkup) Markdown(md string) string { return chatfmt.Telegram(md) }
func (tgMarkup) Pre(s, lang string) string { return chatfmt.TelegramPre(s, lang) }

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
		if f := u.Message.media(); f != nil {
			t.receive(b, u.Message, f)
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

// receive saves a file from the phone in the background, so a download never
// holds up approval taps (updates are handled one at a time).
func (t *Telegram) receive(b *Bridge, msg *tgMessage, f *tgFile) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if f.FileSize > tgDownloadLimit {
			_ = t.Send(ctx, fmt.Sprintf("✗ Telegram only lets bots download files up to 20 MB (this one is %.1f MB).", float64(f.FileSize)/(1<<20)))
			return
		}
		b.HandleFile(ctx, t, Incoming{Name: f.FileName, Mime: f.MimeType, Size: f.FileSize, Caption: msg.Caption,
			Open: func(ctx context.Context) (io.ReadCloser, error) { return t.client.Download(ctx, f.FileID) }})
	}()
}

// MaxFileBytes is the Bot API upload limit.
func (t *Telegram) MaxFileBytes() int64 { return 50 << 20 }

// SendFile sends any file as a document, so photos keep their quality.
func (t *Telegram) SendFile(ctx context.Context, f OutFile) error {
	fields := map[string]string{"chat_id": strconv.FormatInt(t.chatID, 10)}
	if c := strings.TrimSpace(f.Caption); c != "" {
		fields["caption"] = html.EscapeString(model.Truncate(c, 1000))
		fields["parse_mode"] = "HTML"
	}
	return t.withRetry(ctx, func() error { return t.client.Upload(ctx, "sendDocument", fields, "document", f.Name, f.Path) })
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

// Send splits long text into Telegram-sized messages, each valid HTML on
// its own. If Telegram still refuses the markup, the part goes as plain text
// rather than not at all.
func (t *Telegram) Send(ctx context.Context, text string) error {
	for _, part := range chatfmt.SplitHTML(text, 3900) {
		payload := map[string]any{"chat_id": t.chatID, "text": part, "parse_mode": "HTML", "disable_web_page_preview": true}
		err := t.withRetry(ctx, func() error { return t.client.Call(ctx, "sendMessage", payload, nil) })
		if err != nil && strings.Contains(err.Error(), "can't parse entities") {
			payload = map[string]any{"chat_id": t.chatID, "text": chatfmt.PlainFromHTML(part), "disable_web_page_preview": true}
			err = t.withRetry(ctx, func() error { return t.client.Call(ctx, "sendMessage", payload, nil) })
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// sendRetryDelays: a notification (an agent finished, a skill was installed)
// is worth a couple of retries when the network to Telegram flakes.
var sendRetryDelays = []time.Duration{2 * time.Second, 5 * time.Second}

func (t *Telegram) withRetry(ctx context.Context, call func() error) error {
	err := call()
	for _, d := range sendRetryDelays {
		if err == nil || ctx.Err() != nil {
			return err
		}
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return ctx.Err()
		}
		err = call()
	}
	return err
}

// approvalText shows a shell command as a code block (copyable on
// Telegram); other tools as inline code.
func approvalText(m Markup, a *Approval) string {
	what := m.Code(model.Truncate(a.Req.Describe(), 900))
	if a.Req.IsShell() {
		what = m.Pre(model.Truncate(a.Req.Command, 900), "bash")
	}
	return fmt.Sprintf("🔐 %s wants to run (%s)\n\n%s\n\n📁 %s · id %s",
		m.Bold(a.Req.Agent), m.Escape(a.Req.Tool), what,
		m.Escape(shortWS(a.Req.Workspace)), m.Code(a.ID))
}

func (t *Telegram) PromptApproval(ctx context.Context, a *Approval) error {
	kb := map[string]any{"inline_keyboard": [][]map[string]string{{
		{"text": "✅ Permitir · Allow", "callback_data": "ap:" + a.ID + ":allow"},
		{"text": "❌ Negar · Deny", "callback_data": "ap:" + a.ID + ":deny"},
	}, {
		{"text": "⚡ Permitir sessão · Whole session", "callback_data": "ap:" + a.ID + ":auto"},
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
			return "⚡ Allowed / Permitido (autônomo)"
		}
		return "✅ Allowed / Permitido via " + d.By
	case policy.Deny:
		return "❌ Denied / Negado via " + d.By
	}
	if d.By == "timeout" {
		return "⏱️ No answer / Sem resposta: fallback to terminal"
	}
	return "↩️ " + d.Reason
}
