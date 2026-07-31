//go:build !windows

package pi

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tasercake/pi-connect/core"
)

func collectProcessTerminalEvents(t *testing.T, events <-chan core.Event) []core.Event {
	t.Helper()
	var got []core.Event
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	quiet := time.NewTimer(150 * time.Millisecond)
	if !quiet.Stop() {
		<-quiet.C
	}
	for {
		select {
		case event := <-events:
			if event.Type == core.EventError || event.Type == core.EventResult {
				got = append(got, event)
				quiet.Reset(150 * time.Millisecond)
			}
		case <-quiet.C:
			return got
		case <-timer.C:
			t.Fatal("timed out waiting for process terminal event")
		}
	}
}

func TestBoundedStderrBufferCapsCapturedProcessOutput(t *testing.T) {
	var b boundedStderrBuffer
	payload := strings.Repeat("x", maxCapturedStderrBytes*4)
	if n, err := b.Write([]byte(payload)); err != nil || n != len(payload) {
		t.Fatalf("Write = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
	got := b.String()
	if len(got) > maxCapturedStderrBytes+64 {
		t.Fatalf("captured stderr length = %d, want bounded near %d", len(got), maxCapturedStderrBytes)
	}
	if !strings.Contains(got, "[stderr truncated]") {
		t.Fatalf("bounded stderr missing truncation marker: %q", got[len(got)-64:])
	}
}

func TestPiJSONNonzeroExitEmitsOneErrorNoResult(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "json-exit.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'bounded failure detail' >&2\nexit 7\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newPiSession(context.Background(), script, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send("never replay", nil, nil); err != nil {
		t.Fatal(err)
	}
	got := collectProcessTerminalEvents(t, s.Events())
	if len(got) != 1 || got[0].Type != core.EventError || got[0].Error == nil {
		t.Fatalf("events = %+v, want one EventError", got)
	}
	msg := got[0].Error.Error()
	for _, want := range []string{"pid ", "exit code 7", "bounded failure detail"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestPiJSONSIGKILLEmitsOnePossibleOOMErrorNoResult(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "json-kill.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'before kill' >&2\nkill -9 $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newPiSession(context.Background(), script, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.Send("never replay", nil, nil); err != nil {
		t.Fatal(err)
	}
	got := collectProcessTerminalEvents(t, s.Events())
	if len(got) != 1 || got[0].Type != core.EventError || got[0].Error == nil {
		t.Fatalf("events = %+v, want one EventError", got)
	}
	msg := got[0].Error.Error()
	for _, want := range []string{"pid ", "SIGKILL may indicate OOM", "before kill"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
	if strings.Contains(strings.ToLower(msg), "caused by oom") {
		t.Fatalf("error claims certain OOM: %q", msg)
	}
}

func TestPiRPCUnexpectedZeroExitEmitsOneTransportFailure(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "rpc-zero.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newPiRPCSession(context.Background(), script, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.startProcess(); err != nil {
		t.Fatal(err)
	}
	got := collectProcessTerminalEvents(t, s.Events())
	if len(got) != 1 || got[0].Type != core.EventError || got[0].Error == nil || !s.EventErrorIsTerminal(got[0].Error) {
		t.Fatalf("events = %+v, want one terminal EventError", got)
	}
	msg := got[0].Error.Error()
	for _, want := range []string{"pid ", "exit code 0"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}

func TestPiRPCSIGKILLMetadataExactlyOnce(t *testing.T) {
	tmp := t.TempDir()
	script := filepath.Join(tmp, "rpc-kill.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho 'rpc before kill' >&2\nkill -9 $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	s, err := newPiRPCSession(context.Background(), script, tmp, "", "default", "", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.startProcess(); err != nil {
		t.Fatal(err)
	}
	got := collectProcessTerminalEvents(t, s.Events())
	if len(got) != 1 || got[0].Type != core.EventError || got[0].Error == nil || !s.EventErrorIsTerminal(got[0].Error) {
		t.Fatalf("events = %+v, want one terminal EventError", got)
	}
	msg := got[0].Error.Error()
	for _, want := range []string{"pid ", "SIGKILL may indicate OOM", "rpc before kill"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error %q missing %q", msg, want)
		}
	}
}
