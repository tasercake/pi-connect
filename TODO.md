# TODO: Forward audio as files when speech is disabled

- [x] Confirm working branch and baseline state
- [x] Add failing tests for speech-disabled audio forwarding
- [x] Implement minimal core audio-to-file forwarding for speech-disabled path only
- [x] Add/adjust Telegram caption tests for voice/audio captions
- [x] Implement minimal Telegram caption preservation for voice/audio
- [x] Run targeted tests: `go test ./core ./platform/telegram ./agent/pi`
- [x] Run full test suite: `go test ./...`
- [x] Add speech-enabled regression test proving STT path does not attach raw files
- [x] Run independent subagent verification/review
- [x] Final verify git diff and checklist completion
