package core

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const pendingMessageVersion = 1

// PendingMessageState is encoded in the record filename. A pending message is
// safe to replay. A dispatching message may already have reached the agent and
// must never be replayed automatically.
type PendingMessageState string

const (
	PendingMessageStatePending     PendingMessageState = "pending"
	PendingMessageStateDispatching PendingMessageState = "dispatching"
)

// PendingMessageRecord is one accepted, not-yet-completed queued prompt.
// Attachment bytes are intentionally self-contained. JSON base64 encoding costs
// space, but queue depth is bounded and this avoids a second blob lifecycle.
type PendingMessageRecord struct {
	Version      int                 `json:"version"`
	ID           string              `json:"id"`
	CreatedAt    time.Time           `json:"created_at"`
	Project      string              `json:"project"`
	Platform     string              `json:"platform"`
	SessionKey   string              `json:"session_key"`
	WorkspaceDir string              `json:"workspace_dir,omitempty"`
	MessageID    string              `json:"message_id,omitempty"`
	Content      string              `json:"content"`
	UserID       string              `json:"user_id,omitempty"`
	UserName     string              `json:"user_name,omitempty"`
	MsgPlatform  string              `json:"message_platform,omitempty"`
	ChannelKey   string              `json:"channel_key,omitempty"`
	FromVoice    bool                `json:"from_voice,omitempty"`
	Images       []ImageAttachment   `json:"images"`
	Files        []FileAttachment    `json:"files"`
	State        PendingMessageState `json:"-"`
}

// PendingMessageStore persists queued prompts under dataDir/run. One daemon
// process owns the store; mu only protects concurrent engine goroutines.
type PendingMessageStore struct {
	mu  sync.Mutex
	dir string
}

// NewPendingMessageStore creates and verifies the private spool directory.
func NewPendingMessageStore(dataDir string) (*PendingMessageStore, error) {
	dir := filepath.Join(dataDir, "run", "pending_messages", "v1")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pending messages: create store: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("pending messages: secure store: %w", err)
	}
	probe, err := os.CreateTemp(dir, ".availability-*")
	if err != nil {
		return nil, fmt.Errorf("pending messages: store is not writable: %w", err)
	}
	probePath := probe.Name()
	if err := probe.Close(); err != nil {
		os.Remove(probePath)
		return nil, fmt.Errorf("pending messages: close availability probe: %w", err)
	}
	if err := os.Remove(probePath); err != nil {
		return nil, fmt.Errorf("pending messages: remove availability probe: %w", err)
	}
	return &PendingMessageStore{dir: dir}, nil
}

// Enqueue writes a record before returning it to the in-memory queue. The bool
// is false when the deterministic ID already exists in either durable state.
func (s *PendingMessageStore) Enqueue(record PendingMessageRecord) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	id, err := pendingMessageID(record)
	if err != nil {
		return "", false, err
	}
	if _, err := os.Stat(s.path(id, PendingMessageStatePending)); err == nil {
		return id, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("pending messages: inspect pending record: %w", err)
	}
	if _, err := os.Stat(s.path(id, PendingMessageStateDispatching)); err == nil {
		return id, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", false, fmt.Errorf("pending messages: inspect dispatching record: %w", err)
	}

	record.Version = pendingMessageVersion
	record.ID = id
	record.State = ""
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	} else {
		record.CreatedAt = record.CreatedAt.UTC()
	}
	if err := validatePendingMessageRecord(record, id); err != nil {
		return "", false, fmt.Errorf("pending messages: invalid record: %w", err)
	}
	data, err := json.Marshal(record)
	if err != nil {
		return "", false, fmt.Errorf("pending messages: encode record: %w", err)
	}
	if err := AtomicWriteFile(s.path(id, PendingMessageStatePending), data, 0o600); err != nil {
		return "", false, fmt.Errorf("pending messages: persist record: %w", err)
	}
	return id, true, nil
}

// List returns valid records for one engine/platform. Bad files are quarantined
// independently so one partial record cannot block recovery of other prompts.
func (s *PendingMessageStore) List(project, platform string) ([]PendingMessageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.listLocked(project, platform)
}

// MarkDispatching atomically records that dispatch may have begun.
func (s *PendingMessageStore) MarkDispatching(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validPendingMessageID(id) {
		return fmt.Errorf("pending messages: invalid id %q", id)
	}
	from := s.path(id, PendingMessageStatePending)
	to := s.path(id, PendingMessageStateDispatching)
	if err := os.Rename(from, to); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			if _, statErr := os.Stat(to); statErr == nil {
				return nil
			}
		}
		return fmt.Errorf("pending messages: mark dispatching: %w", err)
	}
	return nil
}

// Remove deletes either durable state for id and is idempotent when absent.
func (s *PendingMessageStore) Remove(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !validPendingMessageID(id) {
		return fmt.Errorf("pending messages: invalid id %q", id)
	}
	for _, state := range []PendingMessageState{PendingMessageStatePending, PendingMessageStateDispatching} {
		if err := os.Remove(s.path(id, state)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("pending messages: remove %s record: %w", state, err)
		}
	}
	return nil
}

// RemoveBySessionKey cancels all records for one raw delivery session key.
func (s *PendingMessageStore) RemoveBySessionKey(project, sessionKey string) (int, error) {
	return s.removeMatching(func(r PendingMessageRecord) bool {
		return r.Project == project && r.SessionKey == sessionKey
	})
}

// RemoveByMessageID cancels records targeted by a platform recall event.
func (s *PendingMessageStore) RemoveByMessageID(project, platform, messageID string) (int, error) {
	return s.removeMatching(func(r PendingMessageRecord) bool {
		return r.Project == project && r.Platform == platform && r.MessageID == messageID
	})
}

// RemoveByProject cancels all records during an intentional project reset.
func (s *PendingMessageStore) RemoveByProject(project string) (int, error) {
	return s.removeMatching(func(r PendingMessageRecord) bool { return r.Project == project })
}

func (s *PendingMessageStore) removeMatching(match func(PendingMessageRecord) bool) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	records, err := s.listLocked("", "")
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, record := range records {
		if !match(record) {
			continue
		}
		if err := os.Remove(s.path(record.ID, record.State)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return removed, fmt.Errorf("pending messages: remove matching record: %w", err)
		}
		removed++
	}
	return removed, nil
}

func (s *PendingMessageStore) listLocked(project, platform string) ([]PendingMessageRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("pending messages: list store: %w", err)
	}
	records := make([]PendingMessageRecord, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		id, state, ok := parsePendingMessageFilename(entry.Name())
		if !ok {
			continue
		}
		path := filepath.Join(s.dir, entry.Name())
		data, readErr := os.ReadFile(path)
		var record PendingMessageRecord
		decodeErr := readErr
		if decodeErr == nil {
			decodeErr = json.Unmarshal(data, &record)
		}
		if decodeErr == nil {
			decodeErr = validatePendingMessageRecord(record, id)
		}
		if decodeErr != nil {
			if quarantineErr := s.quarantineLocked(path); quarantineErr != nil {
				slog.Error("pending messages: quarantine failed", "path", path, "error", quarantineErr)
			}
			slog.Warn("pending messages: quarantined malformed record", "path", path, "error", decodeErr)
			continue
		}
		record.State = state
		if (project == "" || record.Project == project) && (platform == "" || record.Platform == platform) {
			records = append(records, record)
		}
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].CreatedAt.Equal(records[j].CreatedAt) {
			return records[i].ID < records[j].ID
		}
		return records[i].CreatedAt.Before(records[j].CreatedAt)
	})
	return records, nil
}

func (s *PendingMessageStore) quarantineLocked(path string) error {
	for i := 0; i < 10; i++ {
		to := fmt.Sprintf("%s.corrupt.%d.%d", path, time.Now().UnixNano(), i)
		if err := os.Rename(path, to); err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return fmt.Errorf("could not allocate quarantine filename")
}

func (s *PendingMessageStore) path(id string, state PendingMessageState) string {
	return filepath.Join(s.dir, id+"."+string(state)+".json")
}

func pendingMessageID(record PendingMessageRecord) (string, error) {
	if record.MessageID != "" {
		sum := sha256.Sum256([]byte(strings.Join([]string{record.Project, record.Platform, record.SessionKey, record.MessageID}, "\x00")))
		return hex.EncodeToString(sum[:]), nil
	}
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("pending messages: generate id: %w", err)
	}
	return hex.EncodeToString(raw[:]), nil
}

func validPendingMessageID(id string) bool {
	if len(id) != 32 && len(id) != 64 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

func parsePendingMessageFilename(name string) (string, PendingMessageState, bool) {
	for _, state := range []PendingMessageState{PendingMessageStatePending, PendingMessageStateDispatching} {
		suffix := "." + string(state) + ".json"
		if strings.HasSuffix(name, suffix) {
			id := strings.TrimSuffix(name, suffix)
			return id, state, validPendingMessageID(id)
		}
	}
	return "", "", false
}

func validatePendingMessageRecord(record PendingMessageRecord, filenameID string) error {
	if record.Version != pendingMessageVersion {
		return fmt.Errorf("unsupported version %d", record.Version)
	}
	if record.ID == "" || record.ID != filenameID || !validPendingMessageID(record.ID) {
		return fmt.Errorf("id does not match filename")
	}
	if record.CreatedAt.IsZero() || record.Project == "" || record.Platform == "" || record.SessionKey == "" {
		return fmt.Errorf("missing required routing metadata")
	}
	return nil
}
