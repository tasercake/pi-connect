package core

import (
	"fmt"
	"testing"
)

type testOutcomeUnknownError struct{}

func (testOutcomeUnknownError) Error() string {
	return "raw prompt acceptance outcome unknown [id=secret]"
}
func (testOutcomeUnknownError) OutcomeUnknown() bool { return true }

func TestUserFacingAgentErrorDoesNotClaimUnknownRequestFailed(t *testing.T) {
	e := NewEngine("test", nil, nil, "", LangEnglish)
	got := e.userFacingAgentError(fmt.Errorf("send wrapper: %w", testOutcomeUnknownError{}))
	want := e.i18n.T(MsgAgentOutcomeUnknown)
	if got != want {
		t.Fatalf("userFacingAgentError() = %q, want %q", got, want)
	}
}
