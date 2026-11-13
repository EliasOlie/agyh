package sync

import (
	"time"

	"github.com/EliasOlie/agyh/internal/model"
	"github.com/EliasOlie/agyh/internal/store"
)

// ManifestVersion is the format version of the cloud vault manifest.
const ManifestVersion = 1

// SessionSummary is a lightweight descriptor of a session in the cloud vault.
type SessionSummary struct {
	Agent     string    `json:"agent"`
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Workspace string    `json:"workspace,omitempty"`
	StartedAt time.Time `json:"started_at"`
	UpdatedAt time.Time `json:"updated_at"`
	Messages  int       `json:"messages"`
	Status    string    `json:"status,omitempty"`
	Parent    string    `json:"parent,omitempty"`
	Hash      string    `json:"hash,omitempty"`
}

func (s SessionSummary) Key() string {
	return s.Agent + ":" + s.ID
}

// Manifest is the index of all sessions and projects stored in the cloud vault.
type Manifest struct {
	Version         int                           `json:"version"`
	UpdatedAt       time.Time                     `json:"updated_at"`
	Sessions        map[string]SessionSummary     `json:"sessions"`
	Projects        []store.Project               `json:"projects,omitempty"`
	ProjectSessions map[string][]string           `json:"project_sessions,omitempty"` // agent:id -> [projects]
	Notes           map[string]map[string]string  `json:"notes,omitempty"`            // agent:id -> (project -> note)
}

func NewManifest() *Manifest {
	return &Manifest{
		Version:         ManifestVersion,
		UpdatedAt:       time.Now(),
		Sessions:        make(map[string]SessionSummary),
		Projects:        nil,
		ProjectSessions: make(map[string][]string),
		Notes:           make(map[string]map[string]string),
	}
}

// Bundle is the complete serialized payload of one session uploaded to or downloaded from cloud.
type Bundle struct {
	Session  model.Session     `json:"session"`
	Messages []model.Message   `json:"messages"`
	Projects []string          `json:"projects,omitempty"`
	Notes    map[string]string `json:"notes,omitempty"` // project -> note
}

// Diff holds the calculated differences between the local store and the cloud vault.
type Diff struct {
	ToUpload   []model.Session
	ToDownload []SessionSummary
}

// StatusReport summarizes the synchronization state.
type StatusReport struct {
	Enabled        bool      `json:"enabled"`
	Endpoint       string    `json:"endpoint,omitempty"`
	Bucket         string    `json:"bucket,omitempty"`
	LocalSessions  int       `json:"local_sessions"`
	RemoteSessions int       `json:"remote_sessions"`
	ToUpload       int       `json:"to_upload"`
	ToDownload     int       `json:"to_download"`
	LastSynced     time.Time `json:"last_synced,omitempty"`
}
