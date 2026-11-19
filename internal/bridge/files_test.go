package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EliasOlie/agyh/internal/policy"
)

// fileChannel is a fakeChannel that also takes files.
type fileChannel struct {
	*fakeChannel
	fmu   sync.Mutex
	files []OutFile
}

func (f *fileChannel) MaxFileBytes() int64 { return 50 << 20 }
func (f *fileChannel) SendFile(_ context.Context, out OutFile) error {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	f.files = append(f.files, out)
	return nil
}
func (f *fileChannel) sentFiles() []OutFile {
	f.fmu.Lock()
	defer f.fmu.Unlock()
	return append([]OutFile(nil), f.files...)
}

type filesFixture struct {
	b       *Bridge
	ch      *fileChannel
	home    string
	ws      string
	outside string
}

// newFilesBridge: a home with a workspace, a folder outside it, and a
// temp dir of its own (the real one, where t.TempDir lives, is allowed).
func newFilesBridge(t *testing.T) filesFixture {
	t.Helper()
	b, _, _, cfg := newBridge(t, 5)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("TMPDIR", filepath.Join(home, "tmp"))
	f := filesFixture{b: b, home: home, ws: filepath.Join(home, "work", "app"), outside: filepath.Join(home, "Documents")}
	for _, d := range []string{f.ws, f.outside, filepath.Join(home, "tmp")} {
		os.MkdirAll(d, 0o700)
	}
	cfg.Workspaces = []string{f.ws}
	b.inbox = filepath.Join(home, "data", "inbox")
	b.protected = []string{filepath.Join(home, "data")}
	f.ch = &fileChannel{fakeChannel: newFake("telegram")}
	b.AddChannel(f.ch)
	return f
}

func putFile(t *testing.T, path, body string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSendFilesInsideWorkspaceGoStraight(t *testing.T) {
	f := newFilesBridge(t)
	putFile(t, filepath.Join(f.ws, "relatorio.pdf"), "%PDF-1.7")
	putFile(t, filepath.Join(f.ws, ".env"), "TOKEN=x")
	res := f.b.SendFiles(context.Background(), FileRequest{Paths: []string{"relatorio.pdf", ".env"}, Cwd: f.ws, Caption: "pronto"})
	if res[0].Error != "" || len(res[0].Sent) != 1 || res[0].Sent[0] != "telegram" {
		t.Fatalf("pdf: %+v", res[0])
	}
	if !strings.Contains(res[1].Error, "never sent") {
		t.Fatalf(".env: %+v", res[1])
	}
	got := f.ch.sentFiles()
	if len(got) != 1 || got[0].Name != "relatorio.pdf" || got[0].Caption != "pronto" || got[0].Mime != "application/pdf" {
		t.Fatalf("sent: %+v", got)
	}
	select {
	case a := <-f.ch.prompted:
		t.Fatalf("asked for approval: %+v", a.Req)
	default:
	}
}

// One approval covers every file outside the workspace; a denial sends none.
func TestSendFilesOutsideNeedsApproval(t *testing.T) {
	for _, allow := range []bool{true, false} {
		f := newFilesBridge(t)
		a := putFile(t, filepath.Join(f.outside, "a.png"), "png")
		b := putFile(t, filepath.Join(f.outside, "b.png"), "png")
		done := make(chan []FileResult)
		go func() {
			done <- f.b.SendFiles(context.Background(), FileRequest{Paths: []string{a, b}, Cwd: f.ws, Agent: "claude", SessionID: "s1"})
		}()
		ap := <-f.ch.prompted
		if ap.Req.Tool != "send_file" || !strings.Contains(ap.Req.Summary, "a.png") || !strings.Contains(ap.Req.Summary, "b.png") {
			t.Fatalf("approval: %+v", ap.Req)
		}
		v := policy.Deny
		if allow {
			v = policy.Allow
		}
		f.b.Decide(context.Background(), ap.ID, Decision{Verdict: v, By: "telegram", Reason: "answered on telegram"}, false)
		res := <-done
		sent := len(f.ch.sentFiles())
		if allow && (sent != 2 || res[0].Error != "" || res[1].Error != "") {
			t.Fatalf("allowed: %d sent, %+v", sent, res)
		}
		if !allow && (sent != 0 || !strings.Contains(res[0].Error, "not approved")) {
			t.Fatalf("denied: %d sent, %+v", sent, res)
		}
	}
}

func TestFilesEndpointNeedsTheSecret(t *testing.T) {
	f := newFilesBridge(t)
	srv := httptest.NewServer(f.b.Handler())
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/files", "application/json", strings.NewReader(`{"paths":["/etc/hostname"]}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestGetSendsToTheAskingChat(t *testing.T) {
	f := newFilesBridge(t)
	other := &fileChannel{fakeChannel: newFake("whatsapp")}
	f.b.AddChannel(other)
	putFile(t, filepath.Join(f.outside, "contrato.pdf"), "%PDF")
	f.b.HandleText(context.Background(), f.ch, "/get ~/Documents/contrato.pdf")
	waitFor(t, func() bool { return len(f.ch.sentFiles()) == 1 })
	if len(other.sentFiles()) != 0 {
		t.Fatal("/get sent to a chat that did not ask")
	}
	if !strings.Contains(f.ch.last(), "contrato.pdf") {
		t.Fatalf("reply: %q", f.ch.last())
	}
	os.MkdirAll(filepath.Join(f.home, "data"), 0o700)
	putFile(t, filepath.Join(f.home, "data", "index.db"), "x")
	f.b.HandleText(context.Background(), f.ch, "/get ~/data/index.db")
	if !strings.Contains(f.ch.last(), "never sent") {
		t.Fatalf("agyh's data went out: %q", f.ch.last())
	}
}

// A photo from the phone lands in the inbox; the next /new takes it along,
// and Claude Code is allowed to read that folder.
func TestReceivedFileGoesAlongWithNew(t *testing.T) {
	f := newFilesBridge(t)
	runner := &fakeRunner{done: make(chan struct{})}
	f.b.runner = runner
	ctx := context.Background()
	f.b.HandleFile(ctx, f.ch, Incoming{Name: "../quadro.jpg", Mime: "image/jpeg", Size: 4,
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("jpeg")), nil }})
	saved := filepath.Join(f.home, "data", "inbox", time.Now().Format("2006-01-02"), "quadro.jpg")
	if b, err := os.ReadFile(saved); err != nil || string(b) != "jpeg" {
		t.Fatalf("not saved at %s: %v", saved, err)
	}
	if !strings.Contains(f.ch.last(), saved) || !strings.Contains(f.ch.last(), "/new") {
		t.Fatalf("reply: %q", f.ch.last())
	}
	out := f.b.Execute(ctx, waMarkup{}, "telegram", "/new claude transcreva o quadro")
	if !strings.Contains(out, "1 file(s)") {
		t.Fatalf("/new reply: %q", out)
	}
	<-runner.done
	argv := strings.Join(runner.argv, "\x00")
	if !strings.Contains(argv, "transcreva o quadro\n\nFiles I sent from my phone") || !strings.Contains(argv, saved) ||
		!strings.Contains(argv, "--add-dir\x00"+filepath.Dir(saved)) {
		t.Fatalf("argv: %q", runner.argv)
	}
	// Used once: the next /new goes without it.
	runner2 := &fakeRunner{done: make(chan struct{})}
	f.b.runner = runner2
	f.b.Execute(ctx, waMarkup{}, "telegram", "/new claude outra coisa")
	<-runner2.done
	if strings.Contains(strings.Join(runner2.argv, " "), "quadro") {
		t.Fatalf("attachment reused: %q", runner2.argv)
	}
}

func TestReceivedFileWithNewCaption(t *testing.T) {
	f := newFilesBridge(t)
	runner := &fakeRunner{done: make(chan struct{})}
	f.b.runner = runner
	f.b.HandleFile(context.Background(), f.ch, Incoming{Mime: "application/pdf", Caption: "/novo agy resuma este contrato",
		Open: func(context.Context) (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("%PDF")), nil }})
	<-runner.done
	if runner.argv[0] != "agy" || !strings.Contains(runner.argv[len(runner.argv)-1], "resuma este contrato") ||
		!strings.Contains(runner.argv[len(runner.argv)-1], ".pdf") {
		t.Fatalf("argv: %q", runner.argv)
	}
}

func TestReceivedFileTooBig(t *testing.T) {
	f := newFilesBridge(t)
	opened := false
	f.b.HandleFile(context.Background(), f.ch, Incoming{Name: "video.mp4", Size: 80 << 20,
		Open: func(context.Context) (io.ReadCloser, error) { opened = true; return nil, io.EOF }})
	if opened || !strings.Contains(f.ch.last(), "limit") {
		t.Fatalf("opened=%v reply=%q", opened, f.ch.last())
	}
}

// Telegram: files go up as documents (multipart) and come down via getFile,
// and the token never shows in an error.
func TestTelegramFileTransfer(t *testing.T) {
	var mu sync.Mutex
	var upload map[string]string
	var uploaded []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botTOKEN/sendDocument":
			_, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
			mr := multipart.NewReader(r.Body, params["boundary"])
			fields := map[string]string{}
			for {
				p, err := mr.NextPart()
				if err != nil {
					break
				}
				b, _ := io.ReadAll(p)
				if p.FormName() == "document" {
					mu.Lock()
					uploaded = b
					mu.Unlock()
					fields["filename"] = p.FileName()
					continue
				}
				fields[p.FormName()] = string(b)
			}
			mu.Lock()
			upload = fields
			mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 1}})
		case "/botTOKEN/getFile":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "photos/f1.jpg"}})
		case "/file/botTOKEN/photos/f1.jpg":
			w.Write([]byte("JPEGDATA"))
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": true})
		}
	}))
	defer srv.Close()
	c := NewTelegramClient("TOKEN")
	c.BaseURL = srv.URL
	tg := NewTelegram(c, 42)

	path := putFile(t, filepath.Join(t.TempDir(), "relatório.pdf"), "%PDF-data")
	if err := tg.SendFile(context.Background(), OutFile{Path: path, Name: "relatório.pdf", Caption: "a <b> & c"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if upload["chat_id"] != "42" || upload["filename"] != "relatório.pdf" || upload["caption"] != "a &lt;b&gt; &amp; c" ||
		!bytes.Equal(uploaded, []byte("%PDF-data")) {
		t.Fatalf("upload: %v %q", upload, uploaded)
	}
	mu.Unlock()

	rc, err := c.Download(context.Background(), "f1")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(rc)
	rc.Close()
	if string(b) != "JPEGDATA" {
		t.Fatalf("download: %q", b)
	}

	c.BaseURL = "http://127.0.0.1:1"
	old := sendRetryDelays
	sendRetryDelays = nil
	t.Cleanup(func() { sendRetryDelays = old })
	if err := tg.SendFile(context.Background(), OutFile{Path: path, Name: "x.pdf"}); err == nil || strings.Contains(err.Error(), "TOKEN") {
		t.Fatalf("error leaks the token or is missing: %v", err)
	}
}

// A photo message from the owner's chat is saved; strangers' files are ignored.
func TestTelegramReceivesPhotos(t *testing.T) {
	f := newFilesBridge(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/botTOKEN/getFile":
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"file_path": "p/big.jpg"}})
		case "/file/botTOKEN/p/big.jpg":
			w.Write([]byte("BIG"))
		default:
			json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": map[string]any{"message_id": 9}})
		}
	}))
	defer srv.Close()
	c := NewTelegramClient("TOKEN")
	c.BaseURL = srv.URL
	tg := NewTelegram(c, 42)
	photo := []tgFile{{FileID: "small", FileSize: 1}, {FileID: "big", FileSize: 3}}
	tg.handle(context.Background(), f.b, TgUpdate{Message: &tgMessage{Chat: tgChat{ID: 7}, Photo: photo}})
	tg.handle(context.Background(), f.b, TgUpdate{Message: &tgMessage{Chat: tgChat{ID: 42}, Photo: photo, Caption: "quadro"}})
	day := filepath.Join(f.home, "data", "inbox", time.Now().Format("2006-01-02"))
	waitFor(t, func() bool {
		entries, _ := os.ReadDir(day)
		return len(entries) == 1
	})
	entries, _ := os.ReadDir(day)
	b, _ := os.ReadFile(filepath.Join(day, entries[0].Name()))
	if string(b) != "BIG" || !strings.HasSuffix(entries[0].Name(), ".jpg") {
		t.Fatalf("saved %s = %q", entries[0].Name(), b)
	}
}
