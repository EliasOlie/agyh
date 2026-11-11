package bridge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EliasOlie/agyh/internal/policy"
)

// fakeTelegram emulates the few Bot API methods agyh uses.
type fakeTelegram struct {
	mu     sync.Mutex
	calls  map[string][]map[string]any
	nextID int64
}

func (f *fakeTelegram) handler(t *testing.T) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/botTOKEN/") {
			json.NewEncoder(w).Encode(map[string]any{"ok": false, "description": "Unauthorized"})
			return
		}
		method := strings.TrimPrefix(r.URL.Path, "/botTOKEN/")
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.calls[method] = append(f.calls[method], body)
		f.nextID++
		id := f.nextID
		f.mu.Unlock()
		var result any = true
		switch method {
		case "sendMessage":
			result = map[string]any{"message_id": id, "chat": map[string]any{"id": body["chat_id"]}}
		case "getMe":
			result = map[string]any{"id": 1, "username": "agyh_test_bot"}
		}
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
	})
}

func (f *fakeTelegram) count(method string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls[method])
}

func newTelegramTest(t *testing.T, token string) (*Telegram, *fakeTelegram) {
	ft := &fakeTelegram{calls: map[string][]map[string]any{}}
	srv := httptest.NewServer(ft.handler(t))
	t.Cleanup(srv.Close)
	c := NewTelegramClient(token)
	c.BaseURL = srv.URL
	return NewTelegram(c, 42), ft
}

func TestTelegramErrorsNeverLeakToken(t *testing.T) {
	tg, _ := newTelegramTest(t, "SECRET-TOKEN-123")
	_, err := tg.client.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN-123") {
		t.Fatalf("want an error without the token, got %v", err)
	}
	tg.client.BaseURL = "http://127.0.0.1:1" // connection refused
	_, err = tg.client.GetMe(context.Background())
	if err == nil || strings.Contains(err.Error(), "SECRET-TOKEN-123") {
		t.Fatalf("network error leaked the token: %v", err)
	}
}

func TestTelegramApprovalButtons(t *testing.T) {
	b, _, _, _ := newBridge(t, 5)
	tg, ft := newTelegramTest(t, "TOKEN")
	b.AddChannel(tg)
	ctx := context.Background()

	ap := b.approvals.Create("a1b2c3", shellReq("make <deploy> & echo"))
	if err := tg.PromptApproval(ctx, ap); err != nil {
		t.Fatal(err)
	}
	sent := ft.calls["sendMessage"][0]
	text := sent["text"].(string)
	if !strings.Contains(text, "make &lt;deploy&gt; &amp; echo") {
		t.Fatalf("command must be HTML-escaped: %s", text)
	}
	if !strings.Contains(mustJSON(sent["reply_markup"]), "ap:a1b2c3:auto") {
		t.Fatalf("missing buttons: %v", sent["reply_markup"])
	}

	// A button press from another chat is ignored.
	tg.handle(ctx, b, TgUpdate{Callback: &tgCallback{ID: "x", Data: "ap:a1b2c3:allow",
		Message: &tgMessage{Chat: tgChat{ID: 999}}}})
	select {
	case <-ap.Done():
		t.Fatal("approval decided by a foreign chat")
	default:
	}

	// The owner's press decides it and the message gets edited.
	tg.handle(ctx, b, TgUpdate{Callback: &tgCallback{ID: "y", Data: "ap:a1b2c3:deny",
		Message: &tgMessage{Chat: tgChat{ID: 42}}}})
	if d := ap.Decision(); d.Verdict != policy.Deny || d.By != "telegram" {
		t.Fatalf("unexpected decision: %+v", d)
	}
	if ft.count("editMessageText") != 1 || ft.count("answerCallbackQuery") != 1 {
		t.Fatalf("calls: %v", ft.calls)
	}
}

func TestTelegramIgnoresStrangers(t *testing.T) {
	b, _, _, _ := newBridge(t, 5)
	tg, ft := newTelegramTest(t, "TOKEN")
	tg.handle(context.Background(), b, TgUpdate{Message: &tgMessage{Chat: tgChat{ID: 7}, Text: "/chats"}})
	if ft.count("sendMessage") != 0 {
		t.Fatal("replied to a stranger")
	}
	tg.handle(context.Background(), b, TgUpdate{Message: &tgMessage{Chat: tgChat{ID: 42}, Text: "/help"}})
	if ft.count("sendMessage") != 1 {
		t.Fatal("owner got no reply")
	}
}

func TestParseCallback(t *testing.T) {
	if id, a, ok := parseCallback("ap:abc123:auto"); !ok || id != "abc123" || a != "auto" {
		t.Fatal("valid callback rejected")
	}
	for _, bad := range []string{"ap:abc:rm", "x:abc:allow", "ap:abc", ""} {
		if _, _, ok := parseCallback(bad); ok {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
