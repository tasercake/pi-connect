package pi

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/chenhg5/cc-connect/core"
)

func TestPiSession_HandleMessageEnd_Errors(t *testing.T) {
	tests := []struct {
		name           string
		eventJSON      string
		wantContains   []string
		wantNotContains []string
	}{
		{
			name: "Transient WebSocket 1000 Failure",
			eventJSON: `{
				"type": "message_end",
				"message": {
					"role": "assistant",
					"errorMessage": "WebSocket closed 1000",
					"stopReason": "error",
					"details": { "phase": "after_message_stream_start", "requestBytes": 481798 },
					"diagnostics": [{ "type": "provider_transport_failure" }]
				}
			}`,
			wantContains: []string{
				"transient provider transport failure",
				"WebSocket closed 1000",
				"Please try again",
				"stopReason=error",
				"phase=after_message_stream_start",
				"bytes=481798",
				"diag=provider_transport_failure",
			},
		},
		{
			name: "Other provider transport failure",
			eventJSON: `{
				"type": "message_end",
				"message": {
					"role": "assistant",
					"errorMessage": "Connection reset by peer",
					"stopReason": "error",
					"diagnostics": [{ "type": "provider_transport_failure" }]
				}
			}`,
			wantContains: []string{
				"Connection reset by peer",
				"diag=provider_transport_failure",
			},
			wantNotContains: []string{
				"transient provider transport failure",
				"Please try again",
			},
		},
		{
			name: "WebSocket 1000 but not transport failure",
			eventJSON: `{
				"type": "message_end",
				"message": {
					"role": "assistant",
					"errorMessage": "WebSocket closed 1000",
					"diagnostics": [{ "type": "some_other_failure" }]
				}
			}`,
			wantContains: []string{
				"WebSocket closed 1000",
				"diag=some_other_failure",
			},
			wantNotContains: []string{
				"transient provider transport failure",
				"Please try again",
			},
		},
		{
			name: "Diagnostics as single map",
			eventJSON: `{
				"type": "message_end",
				"message": {
					"role": "assistant",
					"errorMessage": "WebSocket closed 1000",
					"diagnostics": { "type": "provider_transport_failure" }
				}
			}`,
			wantContains: []string{
				"transient provider transport failure",
				"diag=provider_transport_failure",
				"Please try again",
			},
		},
		{
			name: "Missing diagnostics",
			eventJSON: `{
				"type": "message_end",
				"message": {
					"role": "assistant",
					"errorMessage": "Simple error"
				}
			}`,
			wantContains: []string{
				"Simple error",
			},
			wantNotContains: []string{
				"[",
				"]",
				"transient",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s, _ := newPiSession(context.Background(), "", "", "", "", "", "", nil)
			s.events = make(chan core.Event, 10) // ensure enough capacity for tests

			var raw map[string]any
			if err := json.Unmarshal([]byte(tt.eventJSON), &raw); err != nil {
				t.Fatalf("failed to unmarshal event: %v", err)
			}

			s.handleEvent(raw)

			select {
			case evt := <-s.events:
				if evt.Type != core.EventError {
					t.Errorf("expected EventError, got %v", evt.Type)
				}
				errStr := evt.Error.Error()
				for _, want := range tt.wantContains {
					if !strings.Contains(errStr, want) {
						t.Errorf("error message %q does not contain %q", errStr, want)
					}
				}
				for _, notWant := range tt.wantNotContains {
					if strings.Contains(errStr, notWant) {
						t.Errorf("error message %q contains unexpected %q", errStr, notWant)
					}
				}
			case <-time.After(time.Second):
				t.Error("timed out waiting for event")
			}
		})
	}
}
