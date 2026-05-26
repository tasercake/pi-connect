# TODO: Fix Pi / cc-connect reports WebSocket closed 1000 (Issue #8)

- [ ] Implement enhanced error extraction in `agent/pi/session.go`
    - [ ] Extract `stopReason`
    - [ ] Extract `details.phase` and `details.requestBytes` (handle as `float64`)
    - [ ] Extract `diagnostics` (handle as `[]any` or `map[string]any`)
    - [ ] Construct descriptive error message with brackets `[...]`
    - [ ] Implement transient failure detection (WebSocket 1000 + `provider_transport_failure`)
- [ ] Add regression tests in `agent/pi/session_test.go`
    - [ ] Test "WebSocket closed 1000" + `provider_transport_failure` -> Descriptive transient error
    - [ ] Test other error + `provider_transport_failure` -> Standard descriptive error (no "transient" hint)
    - [ ] Test "WebSocket closed 1000" + no `provider_transport_failure` -> Standard descriptive error (no "transient" hint)
    - [ ] Test missing/malformed diagnostics -> Graceful fallback to raw error
- [ ] Verify fix
    - [ ] Run `go test ./agent/pi`
    - [ ] Run `go test ./...`
- [ ] Finalize and PR
    - [ ] Commit changes
    - [ ] Push branch `fix/issue-8-root-cause`
    - [ ] Open PR referencing #8
