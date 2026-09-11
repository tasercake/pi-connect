package core

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func pendingTestRecord(messageID, content string) PendingMessageRecord {
	return PendingMessageRecord{
		CreatedAt:   time.Date(2026, 9, 11, 1, 2, 3, 4, time.UTC),
		Project:     "project",
		Platform:    "test",
		SessionKey:  "test:room:user",
		MessageID:   messageID,
		Content:     content,
		UserID:      "user",
		UserName:    "User",
		MsgPlatform: "test",
		ChannelKey:  "room",
	}
}

func TestPendingMessageStoreRoundTripAndFIFO(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewPendingMessageStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	first := pendingTestRecord("m1", "first")
	first.Images = []ImageAttachment{
		{MimeType: "image/png", Data: []byte{0, 1, 2}, FileName: "one.png"},
		{MimeType: "image/jpeg", Data: []byte{}, FileName: ""},
		{MimeType: "image/gif", Data: nil, FileName: "three.gif"},
	}
	first.Files = []FileAttachment{
		{MimeType: "application/pdf", Data: []byte{3, 4}, FileName: "a.pdf"},
		{MimeType: "text/plain", Data: []byte{}, FileName: ""},
		{MimeType: "application/octet-stream", Data: nil, FileName: "empty.bin"},
	}
	second := pendingTestRecord("m2", "second")
	second.CreatedAt = first.CreatedAt.Add(time.Second)
	second.Images = []ImageAttachment{} // preserve non-nil empty slices

	id1, created, err := store.Enqueue(first)
	if err != nil || !created {
		t.Fatalf("enqueue first: id=%q created=%v err=%v", id1, created, err)
	}
	id2, created, err := store.Enqueue(second)
	if err != nil || !created {
		t.Fatalf("enqueue second: id=%q created=%v err=%v", id2, created, err)
	}

	reopened, err := NewPendingMessageStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	records, err := reopened.List("project", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 2 || records[0].ID != id1 || records[1].ID != id2 {
		t.Fatalf("records not FIFO: %#v", records)
	}
	if records[1].Content != second.Content || !reflect.DeepEqual(records[1].Images, second.Images) || !reflect.DeepEqual(records[1].Files, second.Files) {
		t.Fatalf("text-only record changed: %#v", records[1])
	}
	if records[0].Content != first.Content || !reflect.DeepEqual(records[0].Images, first.Images) || !reflect.DeepEqual(records[0].Files, first.Files) {
		t.Fatalf("attachments changed:\n got images=%#v files=%#v\nwant images=%#v files=%#v", records[0].Images, records[0].Files, first.Images, first.Files)
	}
}

func TestPendingMessageStoreDeterministicDuplicate(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	record := pendingTestRecord("same", "one")
	id1, created, err := store.Enqueue(record)
	if err != nil || !created {
		t.Fatalf("first enqueue: %v %v", created, err)
	}
	record.Content = "duplicate payload"
	id2, created, err := store.Enqueue(record)
	if err != nil || created || id2 != id1 {
		t.Fatalf("duplicate enqueue: id=%q created=%v err=%v", id2, created, err)
	}
	records, _ := store.List("project", "test")
	if len(records) != 1 || records[0].Content != "one" {
		t.Fatalf("duplicate replaced record: %#v", records)
	}
}

func TestPendingMessageStoreMarkDispatchingRenameOnlyAndRemove(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.Enqueue(pendingTestRecord("m1", strings.Repeat("payload", 20)))
	if err != nil {
		t.Fatal(err)
	}
	pendingPath := store.path(id, PendingMessageStatePending)
	before, err := os.ReadFile(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(pendingPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatching(id); err != nil {
		t.Fatal(err)
	}
	if err := store.MarkDispatching(id); err != nil {
		t.Fatalf("idempotent mark: %v", err)
	}
	dispatchingPath := store.path(id, PendingMessageStateDispatching)
	after, err := os.ReadFile(dispatchingPath)
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, _ := os.Stat(dispatchingPath)
	if !bytes.Equal(before, after) {
		t.Fatal("record bytes changed across rename")
	}
	if !os.SameFile(beforeInfo, afterInfo) {
		t.Fatal("record inode changed across same-directory rename")
	}
	if err := store.Remove(id); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dispatchingPath); !os.IsNotExist(err) {
		t.Fatalf("dispatching record remains: %v", err)
	}
}

func TestPendingMessageStoreConstructorRejectsUnavailablePath(t *testing.T) {
	dataDir := t.TempDir()
	path := filepath.Join(dataDir, "run", "pending_messages", "v1")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewPendingMessageStore(dataDir); err == nil {
		t.Fatal("constructor accepted unavailable store path")
	}
}

func TestPendingMessageStorePrivatePermissions(t *testing.T) {
	dataDir := t.TempDir()
	store, err := NewPendingMessageStore(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := store.Enqueue(pendingTestRecord("m1", "secret"))
	if err != nil {
		t.Fatal(err)
	}
	dirInfo, _ := os.Stat(store.dir)
	fileInfo, _ := os.Stat(store.path(id, PendingMessageStatePending))
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("dir mode=%o, want 700", got)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode=%o, want 600", got)
	}
}

func TestPendingMessageStoreQuarantinesMalformedWithoutBlockingGood(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	goodID, _, err := store.Enqueue(pendingTestRecord("good", "good"))
	if err != nil {
		t.Fatal(err)
	}
	badID := strings.Repeat("a", 64)
	badPath := store.path(badID, PendingMessageStatePending)
	if err := os.WriteFile(badPath, []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	records, err := store.List("project", "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].ID != goodID {
		t.Fatalf("good record blocked: %#v", records)
	}
	matches, _ := filepath.Glob(badPath + ".corrupt.*")
	if len(matches) != 1 {
		t.Fatalf("quarantine matches=%v", matches)
	}
}

func TestPendingMessageStoreRemoveHelpers(t *testing.T) {
	store, err := NewPendingMessageStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	a := pendingTestRecord("a", "a")
	b := pendingTestRecord("b", "b")
	b.SessionKey = "other"
	idA, _, _ := store.Enqueue(a)
	idB, _, _ := store.Enqueue(b)
	if err := store.MarkDispatching(idA); err != nil {
		t.Fatal(err)
	}
	removed, err := store.RemoveBySessionKey("project", a.SessionKey)
	if err != nil || removed != 1 {
		t.Fatalf("remove session: count=%d err=%v", removed, err)
	}
	removed, err = store.RemoveByMessageID("project", "test", b.MessageID)
	if err != nil || removed != 1 {
		t.Fatalf("remove message: count=%d err=%v", removed, err)
	}
	if records, _ := store.List("", ""); len(records) != 0 {
		t.Fatalf("records remain: %#v (ids %s %s)", records, idA, idB)
	}
}
