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
	"sync"
	"time"
)

const runtimeLifecycleVersion = 1

var (
	lifecycleRetryInitial = 250 * time.Millisecond
	lifecycleRetryMax     = 30 * time.Second
)

// RuntimeLease is durable evidence that this daemon owns a live interactive
// agent process. It intentionally contains routing/lifecycle metadata only.
type RuntimeLease struct {
	ID             string    `json:"id"`
	OwnerRunID     string    `json:"owner_run_id"`
	Project        string    `json:"project"`
	Platform       string    `json:"platform"`
	SessionKey     string    `json:"session_key"`
	AgentSessionID string    `json:"agent_session_id,omitempty"`
	OperationID    string    `json:"operation_id,omitempty"`
	TurnInFlight   bool      `json:"turn_in_flight"`
	OutcomeUnknown bool      `json:"outcome_unknown"`
	StartedAt      time.Time `json:"started_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

type lifecycleAlert struct {
	ID             string    `json:"id"`
	Kind           string    `json:"kind"` // crash or restart
	Project        string    `json:"project,omitempty"`
	Platform       string    `json:"platform"`
	SessionKey     string    `json:"session_key"`
	LostProcesses  int       `json:"lost_processes,omitempty"`
	ActiveTurns    int       `json:"active_turns,omitempty"`
	OutcomeUnknown bool      `json:"outcome_unknown,omitempty"`
	LeaseIDs       []string  `json:"lease_ids,omitempty"`
	Attempts       int       `json:"attempts,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	SourceRunID    string    `json:"source_run_id,omitempty"`
}

type runtimeLifecycleState struct {
	Version int                       `json:"version"`
	Leases  map[string]RuntimeLease   `json:"leases"`
	Alerts  map[string]lifecycleAlert `json:"alerts"`
}

// RuntimeLifecycleStore atomically persists process leases and lifecycle alerts.
// Alert delivery is intentionally at-least-once: send and durable acknowledgement
// are separate operations, so a crash after Send succeeds but before ack persists
// can produce one duplicate after restart. Removing before successful Send would
// instead permit silent loss.
type RuntimeLifecycleStore struct {
	mu     sync.Mutex
	path   string
	runID  string
	state  runtimeLifecycleState
	claims map[string]bool // in-process delivery claims; durable state remains pending
}

func NewRuntimeLifecycleStore(dataDir string) (*RuntimeLifecycleStore, error) {
	dir := filepath.Join(dataDir, "run")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("runtime lifecycle: create run dir: %w", err)
	}
	runID, err := randomLifecycleID()
	if err != nil {
		return nil, err
	}
	s := &RuntimeLifecycleStore{
		path:   filepath.Join(dir, "runtime_lifecycle.json"),
		runID:  runID,
		state:  runtimeLifecycleState{Version: runtimeLifecycleVersion, Leases: make(map[string]RuntimeLease), Alerts: make(map[string]lifecycleAlert)},
		claims: make(map[string]bool),
	}
	if err := s.load(); err != nil {
		return nil, err
	}
	changed := s.recoverStaleLeases(time.Now().UTC())
	legacyPath := filepath.Join(dir, "restart_notify")
	if imported, err := s.importLegacyRestart(legacyPath); err != nil {
		// Older versions wrote this marker non-atomically. A truncated legacy
		// marker cannot be routed safely, but must not prevent daemon startup.
		slog.Warn("runtime lifecycle: ignoring invalid legacy restart alert", "error", err)
		if renameErr := quarantineLifecycleFile(legacyPath); renameErr != nil {
			slog.Warn("runtime lifecycle: could not quarantine invalid legacy restart alert", "error", renameErr)
		}
	} else {
		changed = changed || imported
	}
	if changed {
		if err := s.persistLocked(); err != nil {
			return nil, err
		}
		if err := os.Remove(legacyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			slog.Warn("runtime lifecycle: durable legacy import completed but marker removal failed", "error", err)
		}
	}
	return s, nil
}

func randomLifecycleID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("runtime lifecycle: random id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

func lifecycleID(parts ...string) string {
	h := sha256.New()
	for _, part := range parts {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}

func (s *RuntimeLifecycleStore) load() error {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("runtime lifecycle: read: %w", err)
	}
	if err := json.Unmarshal(data, &s.state); err != nil {
		// AtomicWriteFile prevents ordinary partial writes, but disk damage or a
		// manually edited file should not put pi-connect in a permanent crash
		// loop. Preserve corrupt evidence for diagnosis and start with empty state.
		if renameErr := quarantineLifecycleFile(s.path); renameErr != nil {
			return fmt.Errorf("runtime lifecycle: decode: %w (quarantine: %v)", err, renameErr)
		}
		slog.Error("runtime lifecycle: quarantined corrupt state", "path", s.path, "error", err)
		s.state = runtimeLifecycleState{Version: runtimeLifecycleVersion, Leases: make(map[string]RuntimeLease), Alerts: make(map[string]lifecycleAlert)}
		return nil
	}
	if s.state.Leases == nil {
		s.state.Leases = make(map[string]RuntimeLease)
	}
	if s.state.Alerts == nil {
		s.state.Alerts = make(map[string]lifecycleAlert)
	}
	s.state.Version = runtimeLifecycleVersion
	return nil
}

func quarantineLifecycleFile(path string) error {
	if _, err := os.Stat(path); err != nil {
		return err
	}
	for i := 0; i < 100; i++ {
		suffix := fmt.Sprintf(".corrupt.%d", time.Now().UnixNano())
		if i > 0 {
			suffix += fmt.Sprintf(".%d", i)
		}
		err := os.Rename(path, path+suffix)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, os.ErrExist) {
			return err
		}
	}
	return fmt.Errorf("runtime lifecycle: no available quarantine filename for %s", path)
}

func (s *RuntimeLifecycleStore) recoverStaleLeases(now time.Time) bool {
	changed := false
	for id, lease := range s.state.Leases {
		if lease.OwnerRunID == s.runID {
			continue
		}
		alertID := lifecycleID("crash", lease.Project, lease.Platform, lease.SessionKey)
		alert := s.state.Alerts[alertID]
		if alert.ID == "" {
			alert = lifecycleAlert{ID: alertID, Kind: "crash", Project: lease.Project, Platform: lease.Platform, SessionKey: lease.SessionKey, CreatedAt: now}
		}
		if !containsString(alert.LeaseIDs, id) {
			alert.LeaseIDs = append(alert.LeaseIDs, id)
			alert.LostProcesses++
			if lease.TurnInFlight {
				alert.ActiveTurns++
			}
		}
		alert.OutcomeUnknown = alert.OutcomeUnknown || lease.OutcomeUnknown || lease.TurnInFlight
		alert.UpdatedAt = now
		s.state.Alerts[alertID] = alert
		delete(s.state.Leases, id)
		changed = true
	}
	return changed
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (s *RuntimeLifecycleStore) importLegacyRestart(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("runtime lifecycle: read legacy restart alert: %w", err)
	}
	var req RestartRequest
	if err := json.Unmarshal(data, &req); err != nil {
		return false, fmt.Errorf("runtime lifecycle: decode legacy restart alert: %w", err)
	}
	if req.Platform == "" || req.SessionKey == "" {
		return false, fmt.Errorf("runtime lifecycle: invalid legacy restart alert")
	}
	s.enqueueRestartLocked(req, time.Now().UTC())
	// Legacy marker was created by previous process, so it is immediately
	// eligible in this run (unlike a marker enqueued before current shutdown).
	id := lifecycleID("restart", req.Project, req.Platform, req.SessionKey)
	alert := s.state.Alerts[id]
	alert.SourceRunID = ""
	s.state.Alerts[id] = alert
	return true, nil
}

func (s *RuntimeLifecycleStore) persistLocked() error {
	data, err := json.MarshalIndent(s.state, "", "  ")
	if err != nil {
		return fmt.Errorf("runtime lifecycle: encode: %w", err)
	}
	if err := AtomicWriteFile(s.path, data, 0o600); err != nil {
		return fmt.Errorf("runtime lifecycle: persist: %w", err)
	}
	return nil
}

func (s *RuntimeLifecycleStore) UpsertLease(lease RuntimeLease) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	lease.OwnerRunID = s.runID
	if lease.ID == "" || lease.Project == "" || lease.Platform == "" || lease.SessionKey == "" {
		return errors.New("runtime lifecycle: lease missing required routing metadata")
	}
	old, existed := s.state.Leases[lease.ID]
	if existed && lease.StartedAt.IsZero() {
		lease.StartedAt = old.StartedAt
	}
	if lease.StartedAt.IsZero() {
		lease.StartedAt = now
	}
	lease.UpdatedAt = now
	s.state.Leases[lease.ID] = lease
	if err := s.persistLocked(); err != nil {
		if existed {
			s.state.Leases[lease.ID] = old
		} else {
			delete(s.state.Leases, lease.ID)
		}
		return err
	}
	return nil
}

func (s *RuntimeLifecycleStore) RemoveLease(id string) error {
	if id == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lease, ok := s.state.Leases[id]
	if !ok || lease.OwnerRunID != s.runID {
		return nil
	}
	delete(s.state.Leases, id)
	if err := s.persistLocked(); err != nil {
		s.state.Leases[id] = lease
		return err
	}
	return nil
}

func (s *RuntimeLifecycleStore) NewLeaseID(project, identity string) string {
	return lifecycleID("lease", s.runID, project, identity)
}

// AssignUnscopedAlerts migrates legacy restart markers only when the caller can
// identify one unambiguous project. Multi-project startup must not call it.
func (s *RuntimeLifecycleStore) AssignUnscopedAlerts(project string) error {
	if project == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	oldAlerts := s.state.Alerts
	s.state.Alerts = make(map[string]lifecycleAlert, len(oldAlerts))
	for id, alert := range oldAlerts {
		s.state.Alerts[id] = alert
	}
	changed := false
	for id, alert := range s.state.Alerts {
		if alert.Project != "" {
			continue
		}
		delete(s.state.Alerts, id)
		alert.Project = project
		alert.ID = lifecycleID(alert.Kind, alert.Project, alert.Platform, alert.SessionKey)
		s.state.Alerts[alert.ID] = alert
		changed = true
	}
	if !changed {
		s.state.Alerts = oldAlerts
		return nil
	}
	if err := s.persistLocked(); err != nil {
		s.state.Alerts = oldAlerts
		return err
	}
	return nil
}

func (s *RuntimeLifecycleStore) pendingAlerts(project, platform string) []lifecycleAlert {
	s.mu.Lock()
	defer s.mu.Unlock()
	alerts := make([]lifecycleAlert, 0)
	for _, alert := range s.state.Alerts {
		if alert.Platform == platform && alert.Project == project {
			alerts = append(alerts, alert)
		}
	}
	sort.Slice(alerts, func(i, j int) bool {
		if alerts[i].CreatedAt.Equal(alerts[j].CreatedAt) {
			return alerts[i].ID < alerts[j].ID
		}
		return alerts[i].CreatedAt.Before(alerts[j].CreatedAt)
	})
	return alerts
}

func (s *RuntimeLifecycleStore) claimNextAlert(project, platform string) (lifecycleAlert, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var candidates []lifecycleAlert
	for _, alert := range s.state.Alerts {
		if alert.Project == project && alert.Platform == platform && !s.claims[alert.ID] && !(alert.Kind == "restart" && alert.SourceRunID == s.runID) {
			candidates = append(candidates, alert)
		}
	}
	if len(candidates) == 0 {
		return lifecycleAlert{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].CreatedAt.Equal(candidates[j].CreatedAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].CreatedAt.Before(candidates[j].CreatedAt)
	})
	alert := candidates[0]
	s.claims[alert.ID] = true
	return alert, true
}

func (s *RuntimeLifecycleStore) releaseAlert(id string) {
	s.mu.Lock()
	delete(s.claims, id)
	s.mu.Unlock()
}

func (s *RuntimeLifecycleStore) recordAttempt(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	alert, ok := s.state.Alerts[id]
	if !ok {
		return nil
	}
	updated := alert
	updated.Attempts++
	updated.UpdatedAt = time.Now().UTC()
	s.state.Alerts[id] = updated
	if err := s.persistLocked(); err != nil {
		s.state.Alerts[id] = alert
		return err
	}
	return nil
}

func (s *RuntimeLifecycleStore) ackAlert(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.state.Alerts[id]; !ok {
		return nil
	}
	alert := s.state.Alerts[id]
	claimed := s.claims[id]
	delete(s.state.Alerts, id)
	delete(s.claims, id)
	if err := s.persistLocked(); err != nil {
		s.state.Alerts[id] = alert
		if claimed {
			s.claims[id] = true
		}
		return err
	}
	return nil
}

func (s *RuntimeLifecycleStore) EnqueueRestart(req RestartRequest) error {
	if req.Project == "" || req.Platform == "" || req.SessionKey == "" {
		return errors.New("runtime lifecycle: restart alert missing required routing metadata")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := lifecycleID("restart", req.Project, req.Platform, req.SessionKey)
	old, existed := s.state.Alerts[id]
	s.enqueueRestartLocked(req, time.Now().UTC())
	if err := s.persistLocked(); err != nil {
		if existed {
			s.state.Alerts[id] = old
		} else {
			delete(s.state.Alerts, id)
		}
		return err
	}
	return nil
}

func (s *RuntimeLifecycleStore) enqueueRestartReady(req RestartRequest) error {
	if req.Project == "" || req.Platform == "" || req.SessionKey == "" {
		return errors.New("runtime lifecycle: restart alert missing required routing metadata")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id := lifecycleID("restart", req.Project, req.Platform, req.SessionKey)
	old, existed := s.state.Alerts[id]
	s.enqueueRestartLocked(req, time.Now().UTC())
	alert := s.state.Alerts[id]
	alert.SourceRunID = ""
	s.state.Alerts[id] = alert
	if err := s.persistLocked(); err != nil {
		if existed {
			s.state.Alerts[id] = old
		} else {
			delete(s.state.Alerts, id)
		}
		return err
	}
	return nil
}

func (s *RuntimeLifecycleStore) enqueueRestartLocked(req RestartRequest, now time.Time) {
	id := lifecycleID("restart", req.Project, req.Platform, req.SessionKey)
	if _, exists := s.state.Alerts[id]; exists {
		return
	}
	s.state.Alerts[id] = lifecycleAlert{ID: id, Kind: "restart", Project: req.Project, Platform: req.Platform, SessionKey: req.SessionKey, CreatedAt: now, UpdatedAt: now, SourceRunID: s.runID}
}
