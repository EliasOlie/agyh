// Package exploretest provides a fake GitHub (API, codeload, raw) for tests.
package exploretest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/EliasOlie/agyh/internal/explore"
)

const (
	ShaHead = "1111111111111111111111111111111111111111"
	ShaV1   = "2222222222222222222222222222222222222222"
)

// TarEntry is one file (or link) in a test archive.
type TarEntry struct {
	Name, Body string
	Mode       int64
	Link       string
}

// TarGz builds a GitHub-style archive (one top folder).
func TarGz(t *testing.T, top string, entries ...TarEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: top + "/" + e.Name, Mode: 0o644, Typeflag: tar.TypeReg, Size: int64(len(e.Body))}
		if e.Mode != 0 {
			h.Mode = e.Mode
		}
		if e.Link != "" {
			h.Typeflag, h.Linkname, h.Size = tar.TypeSymlink, e.Link, 0
		}
		tw.WriteHeader(h)
		if e.Link == "" {
			tw.Write([]byte(e.Body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

// FakeGitHub serves the API, codeload and raw endpoints agyh uses.
type FakeGitHub struct {
	Srv      *httptest.Server
	mu       sync.Mutex
	AuthSeen map[string]string // path -> Authorization header
}

func NewFakeGitHub(t *testing.T) *FakeGitHub {
	t.Helper()
	f := &FakeGitHub{AuthSeen: map[string]string{}}
	skills := TarGz(t, "r-"+ShaHead,
		TarEntry{Name: "README.md", Body: "# repo"},
		TarEntry{Name: "skills/pdf/SKILL.md", Body: "---\nname: pdf\ndescription: Work with PDFs.\n---\nbody\n"},
		TarEntry{Name: "skills/pdf/scripts/run.sh", Body: "echo pdf\n", Mode: 0o755},
		TarEntry{Name: "skills/pdf/link", Link: "/etc/passwd"},
		TarEntry{Name: "skills/web/SKILL.md", Body: "---\nname: web\ndescription: Browse.\n---\n"},
	)
	evil := TarGz(t, "evil-"+ShaHead, TarEntry{Name: "../outside", Body: "x"})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.AuthSeen[r.URL.Path] = r.Header.Get("Authorization")
		f.mu.Unlock()
		switch r.URL.Path {
		case "/api/repos/o/r/commits/HEAD", "/api/repos/o/evil/commits/HEAD", "/api/repos/o/cat/commits/HEAD":
			w.Write([]byte(ShaHead))
		case "/api/repos/o/r/commits/v1":
			w.Write([]byte(ShaV1))
		case "/codeload/o/r/tar.gz/" + ShaHead:
			w.Write(skills)
		case "/codeload/o/evil/tar.gz/" + ShaHead:
			w.Write(evil)
		case "/api/search/repositories":
			q := r.URL.Query().Get("q")
			items := []map[string]any{{"full_name": "o/r", "description": "PDF skills", "stargazers_count": 10}}
			if strings.Contains(q, "claude-skills") {
				items = append(items, map[string]any{"full_name": "z/big", "description": "pdf toolkit", "stargazers_count": 99})
			}
			json.NewEncoder(w).Encode(map[string]any{"items": items})
		case "/raw/o/cat/HEAD/.claude-plugin/marketplace.json":
			w.Write([]byte(`{"name":"cat","plugins":[
				{"name":"pdfplug","description":"PDF skill","source":"./skills/pdf"},
				{"name":"remote","description":"Web skill","source":{"source":"github","repo":"o/r","path":"skills/web"}},
				{"name":"npmthing","description":"pdf via npm","source":{"source":"npm","package":"x"}},
				{"name":"escape","description":"pdf","source":"../../etc"}]}`))
		default:
			http.NotFound(w, r)
		}
	})
	f.Srv = httptest.NewServer(mux)
	t.Cleanup(f.Srv.Close)
	return f
}

func (f *FakeGitHub) Client(token string) *explore.Client {
	return &explore.Client{API: f.Srv.URL + "/api", Codeload: f.Srv.URL + "/codeload", Raw: f.Srv.URL + "/raw", Token: token, HTTP: f.Srv.Client()}
}
