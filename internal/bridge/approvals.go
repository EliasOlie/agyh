package bridge

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/EliasOlie/agyh/internal/hook"
	"github.com/EliasOlie/agyh/internal/policy"
)

// Decision is the outcome of an approval.
type Decision struct {
	Verdict policy.Verdict
	Reason  string
	By      string // "telegram", "whatsapp", "terminal", "auto", "timeout"
}

// Approval is one pending tool call. The first decision wins; later ones are ignored.
type Approval struct {
	ID      string
	Req     hook.Request
	Created time.Time

	once     sync.Once
	done     chan struct{}
	decision Decision

	mu      sync.Mutex
	handles map[string]any // per-channel message reference, so it can be edited later
}

func (a *Approval) Done() <-chan struct{} { return a.done }
func (a *Approval) Decision() Decision    { <-a.done; return a.decision }

func (a *Approval) SetHandle(channel string, h any) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.handles[channel] = h
}

func (a *Approval) Handle(channel string) any {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.handles[channel]
}

// SessionKey identifies the agent session for autonomous mode.
func (a *Approval) SessionKey() string { return a.Req.Agent + ":" + a.Req.SessionID }

var validID = regexp.MustCompile(`^[a-f0-9]{6,32}$`)

// NewID returns a short random id that is easy to type on a phone.
func NewID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Approvals tracks pending approvals and sessions in autonomous mode.
type Approvals struct {
	mu        sync.Mutex
	pending   map[string]*Approval
	auto      map[string]time.Time
	statePath string
}

func NewApprovals(statePath string) *Approvals {
	a := &Approvals{pending: map[string]*Approval{}, auto: map[string]time.Time{}, statePath: statePath}
	a.load()
	return a
}

// Create registers a new approval. id may be supplied by the hook (so it can
// cancel it from the terminal); an invalid or duplicate id is replaced.
func (a *Approvals) Create(id string, req hook.Request) *Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	if !validID.MatchString(id) || a.pending[id] != nil {
		id = NewID()
		for a.pending[id] != nil {
			id = NewID()
		}
	}
	ap := &Approval{ID: id, Req: req, Created: time.Now(), done: make(chan struct{}), handles: map[string]any{}}
	a.pending[id] = ap
	return ap
}

// Resolve records the decision if the approval is still pending.
// It returns the approval and true only for the call that decided it.
func (a *Approvals) Resolve(id string, d Decision) (*Approval, bool) {
	a.mu.Lock()
	ap := a.pending[id]
	if ap != nil {
		delete(a.pending, id)
	}
	a.mu.Unlock()
	if ap == nil {
		return nil, false
	}
	won := false
	ap.once.Do(func() {
		ap.decision = d
		close(ap.done)
		won = true
	})
	return ap, won
}

func (a *Approvals) Pending() []*Approval {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]*Approval, 0, len(a.pending))
	for _, ap := range a.pending {
		out = append(out, ap)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Created.Before(out[j].Created) })
	return out
}

func (a *Approvals) SetAuto(sessionKey string, on bool) {
	a.mu.Lock()
	if on {
		a.auto[sessionKey] = time.Now()
	} else {
		delete(a.auto, sessionKey)
	}
	a.mu.Unlock()
	a.save()
}

func (a *Approvals) IsAuto(sessionKey string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, ok := a.auto[sessionKey]
	return ok
}

func (a *Approvals) AutoSessions() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]string, 0, len(a.auto))
	for k := range a.auto {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

type approvalsState struct {
	Auto map[string]time.Time `json:"auto"`
}

func (a *Approvals) load() {
	if a.statePath == "" {
		return
	}
	b, err := os.ReadFile(a.statePath)
	if err != nil {
		return
	}
	var st approvalsState
	if json.Unmarshal(b, &st) == nil && st.Auto != nil {
		a.auto = st.Auto
	}
}

func (a *Approvals) save() {
	if a.statePath == "" {
		return
	}
	a.mu.Lock()
	b, _ := json.MarshalIndent(approvalsState{Auto: a.auto}, "", "  ")
	a.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(a.statePath), 0o700)
	_ = os.WriteFile(a.statePath, b, 0o600)
}
