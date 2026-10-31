package storage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"trevorblackman.dev/agent-board/internal/domain"
)

// Manifest describes a completed backup artifact.
type Manifest struct {
	CreatedAt            string            `json:"created_at"`
	Engine               string            `json:"engine"`
	SchemaVersion        int               `json:"schema_version"`
	SearchProfileVersion int               `json:"search_profile_version"`
	BoardInstanceID      string            `json:"board_instance_id"`
	GenerationID         string            `json:"generation_id"`
	ContentRevision      int64             `json:"content_revision"`
	HighestSequence      int64             `json:"highest_sequence"`
	ViewTimeHWMUS        int64             `json:"view_time_hwm_us"`
	Counts               map[string]int64  `json:"counts"`
	Files                map[string]string `json:"files_sha256"`
	Complete             bool              `json:"complete"`
}

const manifestName = "manifest.json"

var backupMu sync.Mutex

// ErrBackupRunning is returned when another backup is in progress.
var ErrBackupRunning = errors.New("backup already running")

// Backup writes a consistent copy of the database plus a manifest into a new
// timestamped directory under dir, validates it, then renames it into place.
// It keeps the two newest completed artifacts and removes stale temporaries.
func (db *DB) Backup(ctx context.Context, dir string) (string, *Manifest, error) {
	if !backupMu.TryLock() {
		return "", nil, ErrBackupRunning
	}
	defer backupMu.Unlock()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", nil, err
	}
	if err := checkSpace(dir, db.Path); err != nil {
		return "", nil, err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	tmp := filepath.Join(dir, "tmp-"+stamp)
	final := filepath.Join(dir, stamp)
	if err := os.MkdirAll(tmp, 0o750); err != nil {
		return "", nil, err
	}
	cleanup := func() { os.RemoveAll(tmp) }
	copyPath := filepath.Join(tmp, "board.db")
	conn := db.reads
	if conn == nil {
		conn = db.writer
	}
	if _, err := conn.ExecContext(ctx, `VACUUM INTO ?`, filepath.ToSlash(copyPath)); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("vacuum into: %w", err)
	}
	m, err := verifyCopy(ctx, copyPath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	sum, err := fileSHA256(copyPath)
	if err != nil {
		cleanup()
		return "", nil, err
	}
	m.Files = map[string]string{"board.db": sum}
	m.Complete = true
	mb, _ := json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(filepath.Join(tmp, manifestName), mb, 0o640); err != nil {
		cleanup()
		return "", nil, err
	}
	if err := os.Rename(tmp, final); err != nil {
		cleanup()
		return "", nil, err
	}
	pruneBackups(dir, 2)
	return final, m, nil
}

func verifyCopy(ctx context.Context, path string) (*Manifest, error) {
	c, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer c.Close()
	var ic string
	if err := c.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&ic); err != nil {
		return nil, err
	}
	if ic != "ok" {
		return nil, fmt.Errorf("backup copy failed integrity_check: %s", ic)
	}
	st, err := LoadState(ctx, c)
	if err != nil {
		return nil, err
	}
	hs, err := MaxSequence(ctx, c)
	if err != nil {
		return nil, err
	}
	m := &Manifest{CreatedAt: domain.FormatTime(time.Now()), Engine: "sqlite/modernc.org/sqlite", SchemaVersion: st.SchemaVersion, SearchProfileVersion: st.SearchProfileVersion,
		BoardInstanceID: st.InstanceID, GenerationID: st.GenerationID, ContentRevision: st.ContentRevision, HighestSequence: hs, ViewTimeHWMUS: st.ViewTimeHWMUS, Counts: map[string]int64{}}
	for _, t := range []string{"api_principals", "agents", "execution_contexts", "categories", "tags", "entries", "posts", "comments", "open_events", "view_daily_sessions", "view_daily_workspaces"} {
		var n int64
		if err := c.QueryRowContext(ctx, `SELECT count(*) FROM `+t).Scan(&n); err != nil {
			return nil, err
		}
		m.Counts[t] = n
	}
	return m, nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// ListBackups returns completed backup directories, newest first.
func ListBackups(dir string) ([]string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || strings.HasPrefix(e.Name(), "tmp-") {
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, e.Name(), manifestName)); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(out)))
	return out, nil
}

func pruneBackups(dir string, keep int) {
	names, err := ListBackups(dir)
	if err != nil {
		return
	}
	for i, n := range names {
		if i >= keep {
			os.RemoveAll(filepath.Join(dir, n))
		}
	}
	// Abandoned temporaries older than 24 hours.
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.IsDir() && strings.HasPrefix(e.Name(), "tmp-") {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				os.RemoveAll(filepath.Join(dir, e.Name()))
			}
		}
	}
}

// ReadManifest loads a backup manifest.
func ReadManifest(path string) (*Manifest, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// VerifyBackup checks a completed artifact's hash and manifest.
func VerifyBackup(dir string) (*Manifest, error) {
	m, err := ReadManifest(filepath.Join(dir, manifestName))
	if err != nil {
		return nil, err
	}
	if !m.Complete {
		return nil, errors.New("manifest is not marked complete")
	}
	for name, want := range m.Files {
		got, err := fileSHA256(filepath.Join(dir, name))
		if err != nil {
			return nil, err
		}
		if got != want {
			return nil, fmt.Errorf("%s hash mismatch", name)
		}
	}
	return m, nil
}
