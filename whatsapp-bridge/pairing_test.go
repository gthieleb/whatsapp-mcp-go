package main

import (
	"context"
	"testing"
	"time"

	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"

	"whatsapp-bridge/wastate"
)

// testLogger roars only on errors (keeps the terminal QR prints from the
// code events as the only test noise).
func testLogger() waLog.Logger { return waLog.Stdout("pairing-test", "ERROR", false) }

// scriptedPairer mirrors the whatsmeow pairing lifecycle per attempt: each
// GetQRChannel call hands out a fresh buffered channel with the scripted
// items (whatsmeow closes the channel on terminal events — timeout,
// success — so a fully-buffered channel models one complete attempt).
type scriptedPairer struct {
	getErrs  []error
	connErrs []error
	scripts  [][]whatsmeow.QRChannelItem

	attempts   int
	okAttempts int
}

func (s *scriptedPairer) GetQRChannel(context.Context) (<-chan whatsmeow.QRChannelItem, error) {
	idx := s.attempts
	s.attempts++
	if idx < len(s.getErrs) && s.getErrs[idx] != nil {
		return nil, s.getErrs[idx]
	}
	scriptIdx := s.okAttempts
	if scriptIdx >= len(s.scripts) {
		return nil, whatsmeow.ErrClientIsNil
	}
	s.okAttempts++
	out := make(chan whatsmeow.QRChannelItem, len(s.scripts[scriptIdx]))
	for _, it := range s.scripts[scriptIdx] {
		out <- it
	}
	close(out)
	return out, nil
}

func (s *scriptedPairer) Connect() error {
	idx := s.attempts - 1
	if idx >= 0 && idx < len(s.connErrs) && s.connErrs[idx] != nil {
		return s.connErrs[idx]
	}
	return nil
}

func codeEvt(c string) whatsmeow.QRChannelItem {
	return whatsmeow.QRChannelItem{Event: whatsmeow.QRChannelEventCode, Code: c}
}

// Test_runPairingSupervisor_rearms_on_timeout covers the W5.12a
// refinement: a timed-out QR session does not kill the process — the
// supervisor marks stale, drops the dead PNG and re-arms a fresh session
// on the same client (whatsmeow disconnects itself on terminal events,
// qrchan.go:66-71, so the re-arm preconditions hold).
func Test_runPairingSupervisor_rearms_on_timeout(t *testing.T) {
	p := &scriptedPairer{
		scripts: [][]whatsmeow.QRChannelItem{
			{codeEvt("c1"), whatsmeow.QRChannelTimeout},
			{codeEvt("c2"), whatsmeow.QRChannelSuccess},
		},
	}
	st := wastate.New()

	// stub the encode path? No — code events encode PNG of the code
	// content directly; "c2" encodes fine.
	runPairingSupervisor(context.Background(), p, st, time.Millisecond, testLogger())

	if p.attempts != 2 {
		t.Fatalf("expected re-arm (2 attempts), got %d", p.attempts)
	}
	if st.QRStale() {
		t.Fatal("stale flag must clear when the fresh code arrives")
	}
	if st.PairingQRPNG() == nil {
		t.Fatal("fresh qr png must be served after re-arm")
	}
}

// Test_runPairingSupervisor_timeout_marks_stale runs one attempt only
// (ctx cancels during the post-timeout backoff) to assert the mid-run
// semantics directly: stale=true, dead PNG gone.
func Test_runPairingSupervisor_timeout_marks_stale_and_clears_png(t *testing.T) {
	p := &scriptedPairer{
		scripts: [][]whatsmeow.QRChannelItem{
			{codeEvt("c1"), whatsmeow.QRChannelTimeout},
		},
	}
	st := wastate.New()
	st.SetPairingQRPNG([]byte("old-dead"))
	st.SetQRStale(false)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runPairingSupervisor(ctx, p, st, time.Millisecond, testLogger())

	if !st.QRStale() {
		t.Fatal("stale flag must live until a fresh code arrives")
	}
	if st.PairingQRPNG() != nil {
		t.Fatal("dead png must not be served")
	}
}

// Test_runPairingSupervisor_success_stops supervising: pairing success
// returns immediately (no further attempt).
func Test_runPairingSupervisor_success_stops(t *testing.T) {
	p := &scriptedPairer{
		scripts: [][]whatsmeow.QRChannelItem{
			{codeEvt("c1"), whatsmeow.QRChannelSuccess},
		},
	}
	st := wastate.New()

	runPairingSupervisor(context.Background(), p, st, time.Millisecond, testLogger())

	if p.attempts != 1 {
		t.Fatalf("supervisor must return on success, attempts=%d", p.attempts)
	}
}

// Test_runPairingSupervisor_get_errors_retries covers per-attempt
// GetQRChannel failures (transient socket state): back off and retry.
func Test_runPairingSupervisor_get_errors_retries(t *testing.T) {
	p := &scriptedPairer{
		getErrs: []error{whatsmeow.ErrClientIsNil},
		scripts: [][]whatsmeow.QRChannelItem{
			{codeEvt("c1"), whatsmeow.QRChannelSuccess},
		},
	}
	st := wastate.New()

	runPairingSupervisor(context.Background(), p, st, time.Millisecond, testLogger())

	if p.attempts != 2 {
		t.Fatalf("expected retry after GetQRChannel error, attempts=%d", p.attempts)
	}
	if st.PairingQRPNG() == nil {
		t.Fatal("png must be set after the successful retry")
	}
}
