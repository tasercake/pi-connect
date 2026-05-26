# Fix Plan: Pi / cc-connect reports WebSocket closed 1000 (Issue #8)

## Goal
Improve the diagnostics and user experience when the Pi provider closes the WebSocket connection prematurely (specifically with close code 1000), which is identified as a transient provider transport failure.

## Root Cause
The Pi provider occasionally closes the WebSocket connection with code `1000` during large resumed requests. The current implementation of the Pi agent adapter in `cc-connect` only extracts the top-level `errorMessage` from the `message_end` event, resulting in an opaque error message: `❌ Error: WebSocket closed 1000`.

## Proposed Changes

### 1. Enhance Error Extraction in `agent/pi/session.go`
Modify the `handleMessageEnd` function to extract richer diagnostic data from the Pi JSON event.

- **Target**: `agent/pi/session.go` -> `handleMessageEnd()`
- **Extraction Logic**:
    - Extract `stopReason` (string).
    - Extract `details` (map), specifically `phase` (string) and `requestBytes` (float64/int).
    - Extract `diagnostics` (array of maps or single map), specifically the `type` field.
- **Formatting**:
    - Build a list of diagnostic details (e.g., `stopReason=error`, `phase=after_message_stream_start`, `bytes=481798`, `diag=provider_transport_failure`).
    - Append these details in brackets `[...]` to the primary `errorMessage`.
- **Transient Failure Detection**:
    - If `errorMessage` contains "WebSocket closed 1000" AND a diagnostic of type `provider_transport_failure` is present:
        - Prefix the error with: `"transient provider transport failure: "`
        - Suffix the error with: `". Please try again."`

### 2. Verification & Testing
Since this is a data-parsing change, it can be verified with a unit test simulating the Pi JSON output.

- **Test File**: `agent/pi/session_test.go` (create if not exists, otherwise add to existing tests).
- **Test Case**:
    - Feed a `message_end` event into `handleEvent` with:
        ```json
        {
          "type": "message_end",
          "message": {
            "role": "assistant",
            "errorMessage": "WebSocket closed 1000",
            "stopReason": "error",
            "details": { "phase": "after_message_stream_start", "requestBytes": 481798 },
            "diagnostics": [{ "type": "provider_transport_failure" }]
          }
        }
        ```
    - Assert that the emitted `core.EventError` contains the string `"transient provider transport failure"` and `"Please try again."`, as well as the diagnostic details.

## Scope
This fix is surgical and limited to the `agent/pi` package. No changes are required in `core/` or other adapters.

## Regression Risk
Low. The changes only affect how error messages are constructed when `errorMessage` is already present. It does not change the flow of events or the session lifecycle.
