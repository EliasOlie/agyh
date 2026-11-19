package bridge

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/mdp/qrterminal/v3"

	"github.com/EliasOlie/agyh/internal/chatfmt"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	"google.golang.org/protobuf/proto"
	"modernc.org/sqlite"
)

// WhatsApp talks to you through your own "Message yourself" chat, using
// agyh as a linked device (like WhatsApp Web). This is an unofficial client:
// use a number you are comfortable linking, at your own risk.
type WhatsApp struct {
	client *whatsmeow.Client
	self   types.JID

	mu   sync.Mutex
	sent map[types.MessageID]bool // our own outgoing messages, so we don't treat them as commands
}

func init() {
	// whatsmeow's sqlstore expects a "sqlite3" driver; register the pure-Go one under that name.
	for _, d := range sql.Drivers() {
		if d == "sqlite3" {
			return
		}
	}
	sql.Register("sqlite3", &sqlite.Driver{})
}

func openDevice(ctx context.Context, dbPath string) (*sqlstore.Container, *whatsmeow.Client, error) {
	dsn := "file:" + dbPath + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
	container, err := sqlstore.New(ctx, "sqlite3", dsn, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("open whatsapp store: %w", err)
	}
	dev, err := container.GetFirstDevice(ctx)
	if err != nil {
		return nil, nil, err
	}
	return container, whatsmeow.NewClient(dev, nil), nil
}

// PairWhatsApp links agyh as a device by printing a QR code in the terminal.
// It returns your JID once the phone scans it.
func PairWhatsApp(ctx context.Context, dbPath string, out io.Writer) (string, error) {
	_, client, err := openDevice(ctx, dbPath)
	if err != nil {
		return "", err
	}
	if client.Store.ID != nil {
		jid := client.Store.ID.ToNonAD().String()
		return jid, nil // already paired
	}
	qrCh, err := client.GetQRChannel(ctx)
	if err != nil {
		return "", err
	}
	if err := client.Connect(); err != nil {
		return "", err
	}
	defer client.Disconnect()
	for item := range qrCh {
		switch item.Event {
		case "code":
			fmt.Fprintln(out, "\nOn your phone: WhatsApp → Settings → Linked devices → Link a device, then scan:")
			qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, out)
		case "success":
			// Give the phone a moment to finish the handshake before we disconnect.
			time.Sleep(3 * time.Second)
			if client.Store.ID == nil {
				return "", errors.New("paired but no device id was stored")
			}
			self := client.Store.ID.ToNonAD()
			_, _ = client.SendMessage(ctx, self, &waE2E.Message{Conversation: proto.String(
				"🤖 agyh conectado com sucesso! / Connected successfully!\n\nEnvie /start ou /ajuda aqui para ver os comandos e como aprovar ações do agente pelo celular.")})
			return self.String(), nil
		case "timeout":
			return "", errors.New("QR code expired: run the command again")
		default:
			if item.Error != nil {
				return "", fmt.Errorf("pairing failed: %w", item.Error)
			}
			return "", fmt.Errorf("pairing failed: %s", item.Event)
		}
	}
	return "", errors.New("pairing aborted")
}

// NewWhatsApp connects an already paired device.
func NewWhatsApp(ctx context.Context, dbPath string) (*WhatsApp, error) {
	_, client, err := openDevice(ctx, dbPath)
	if err != nil {
		return nil, err
	}
	if client.Store.ID == nil {
		return nil, errors.New("whatsapp is not paired: run `agyh connect whatsapp`")
	}
	return &WhatsApp{client: client, self: client.Store.ID.ToNonAD(), sent: map[types.MessageID]bool{}}, nil
}

func (w *WhatsApp) Name() string   { return "whatsapp" }
func (w *WhatsApp) Markup() Markup { return waMarkup{} }

type waMarkup struct{}

func (waMarkup) Bold(s string) string      { return "*" + s + "*" }
func (waMarkup) Code(s string) string      { return "`" + strings.ReplaceAll(s, "`", "'") + "`" }
func (waMarkup) Escape(s string) string    { return s }
func (waMarkup) Markdown(md string) string { return chatfmt.WhatsApp(md) }
func (waMarkup) Pre(s, _ string) string    { return chatfmt.WhatsAppPre(s) }

func (w *WhatsApp) Run(ctx context.Context, b *Bridge) error {
	w.client.AddEventHandler(func(evt any) {
		msg, ok := evt.(*events.Message)
		if !ok || !w.isSelfChat(msg.Info) {
			return
		}
		w.mu.Lock()
		ours := w.sent[msg.Info.ID]
		delete(w.sent, msg.Info.ID)
		w.mu.Unlock()
		if ours {
			return
		}
		if in, ok := w.incoming(msg); ok {
			go func() {
				fctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
				defer cancel()
				b.HandleFile(fctx, w, in)
			}()
			return
		}
		text := msg.Message.GetConversation()
		if text == "" {
			text = msg.Message.GetExtendedTextMessage().GetText()
		}
		if strings.TrimSpace(text) != "" {
			go b.HandleText(ctx, w, text)
		}
	})
	if err := w.client.Connect(); err != nil {
		return err
	}
	<-ctx.Done()
	w.client.Disconnect()
	return ctx.Err()
}

// Only messages you send to yourself count as commands; other chats are ignored.
func (w *WhatsApp) isSelfChat(info types.MessageInfo) bool {
	if !info.IsFromMe || info.IsGroup {
		return false
	}
	if info.Chat.User == w.self.User {
		return true
	}
	lid := w.client.Store.LID
	return !lid.IsEmpty() && info.Chat.User == lid.User
}

func (w *WhatsApp) Send(ctx context.Context, text string) error {
	for _, part := range chatfmt.SplitText(text, 3500) {
		// The robot marks agyh's messages; a block or quote must start its own line.
		sep := " "
		if strings.HasPrefix(part, "```") || strings.HasPrefix(part, ">") {
			sep = "\n"
		}
		resp, err := w.client.SendMessage(ctx, w.self, &waE2E.Message{Conversation: proto.String("🤖" + sep + part)})
		if err != nil {
			return err
		}
		w.mu.Lock()
		w.sent[resp.ID] = true
		w.mu.Unlock()
	}
	return nil
}

// incoming turns a media message from your self-chat into a file to save.
func (w *WhatsApp) incoming(msg *events.Message) (Incoming, bool) {
	m := msg.Message
	var dl whatsmeow.DownloadableMessage
	in := Incoming{}
	switch {
	case m.GetDocumentMessage() != nil:
		d := m.GetDocumentMessage()
		dl, in = d, Incoming{Name: d.GetFileName(), Mime: d.GetMimetype(), Size: int64(d.GetFileLength()), Caption: d.GetCaption()}
	case m.GetImageMessage() != nil:
		d := m.GetImageMessage()
		dl, in = d, Incoming{Mime: d.GetMimetype(), Size: int64(d.GetFileLength()), Caption: d.GetCaption()}
	case m.GetVideoMessage() != nil:
		d := m.GetVideoMessage()
		dl, in = d, Incoming{Mime: d.GetMimetype(), Size: int64(d.GetFileLength()), Caption: d.GetCaption()}
	case m.GetAudioMessage() != nil:
		d := m.GetAudioMessage()
		dl, in = d, Incoming{Mime: d.GetMimetype(), Size: int64(d.GetFileLength())}
	default:
		return Incoming{}, false
	}
	in.Open = func(ctx context.Context) (io.ReadCloser, error) {
		data, err := w.client.Download(ctx, dl)
		if err != nil {
			return nil, err
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	}
	return in, true
}

// MaxFileBytes: WhatsApp takes documents up to 100 MB (agyh's files.max_mb still applies).
func (w *WhatsApp) MaxFileBytes() int64 { return 100 << 20 }

// SendFile sends any file as a document, so photos keep their quality.
func (w *WhatsApp) SendFile(ctx context.Context, f OutFile) error {
	data, err := os.ReadFile(f.Path)
	if err != nil {
		return err
	}
	up, err := w.client.Upload(ctx, data, whatsmeow.MediaDocument)
	if err != nil {
		return err
	}
	doc := &waE2E.DocumentMessage{
		URL: proto.String(up.URL), DirectPath: proto.String(up.DirectPath), MediaKey: up.MediaKey,
		FileEncSHA256: up.FileEncSHA256, FileSHA256: up.FileSHA256, FileLength: proto.Uint64(up.FileLength),
		Mimetype: proto.String(f.Mime), FileName: proto.String(f.Name), Title: proto.String(f.Name),
	}
	if c := strings.TrimSpace(f.Caption); c != "" {
		doc.Caption = proto.String(c)
	}
	resp, err := w.client.SendMessage(ctx, w.self, &waE2E.Message{DocumentMessage: doc})
	if err != nil {
		return err
	}
	w.mu.Lock()
	w.sent[resp.ID] = true
	w.mu.Unlock()
	return nil
}

func (w *WhatsApp) PromptApproval(ctx context.Context, a *Approval) error {
	m := w.Markup()
	return w.Send(ctx, approvalText(m, a)+fmt.Sprintf(
		"\n\nResponda / Reply:\n• %s ou %s: aprovar / allow\n• %s ou %s: rejeitar / deny\n• %s ou %s: liberar sessão inteira / whole session ⚡",
		m.Code("s "+a.ID), m.Code("y "+a.ID),
		m.Code("n "+a.ID), m.Code("nao "+a.ID),
		m.Code("a "+a.ID), m.Code("auto "+a.ID)))
}

func (w *WhatsApp) ApprovalResolved(ctx context.Context, a *Approval, d Decision) error {
	return w.Send(ctx, w.Markup().Code(a.ID)+" "+outcome(d))
}
