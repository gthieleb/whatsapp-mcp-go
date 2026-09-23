package wastate

import (
	"sync"
	"testing"
)

// TestQRStaleLifecycle covers the QR-stale flag introduced with the
// pairing-timeout freeze fix (eventlab plan W5.12a): after a QR timeout the
// bridge must mark the session stale and clear it when a fresh QR arrives.
func TestQRStaleLifecycle(t *testing.T) {
	s := New()
	if s.QRStale() {
		t.Fatal("fresh state must not be stale")
	}

	// Fresh QR available, not stale: pairing required, QR served.
	s.SetPairingQRPNG([]byte("png-1"))
	if !s.PairingRequired() {
		t.Fatal("pairing required when QR present and not logged in")
	}
	if s.PairingQRPNG() == nil {
		t.Fatal("QR png must be served while session is fresh")
	}

	// Timeout: mark stale + clear the dead QR (caller does both).
	s.SetQRStale(true)
	s.ClearPairingQR()
	if !s.QRStale() {
		t.Fatal("stale flag must survive ClearPairingQR")
	}
	if s.PairingQRPNG() != nil {
		t.Fatal("stale session must not serve a dead QR png")
	}
	if s.PairingRequired() {
		t.Fatal("stale session without QR must not report pairing_required")
	}

	// New pairing attempt delivers a fresh code: clear stale.
	s.SetQRStale(false)
	s.SetPairingQRPNG([]byte("png-2"))
	if s.QRStale() {
		t.Fatal("stale flag must clear on fresh QR")
	}
	if string(s.PairingQRPNG()) != "png-2" {
		t.Fatalf("fresh QR bytes wrong: got %q", s.PairingQRPNG())
	}
}

// TestQRStaleConcurrent guards the mutex contract while a QR-fetch handler
// races a pairing goroutine writing the stale flag.
func TestQRStaleConcurrent(t *testing.T) {
	s := New()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.SetQRStale(true)
			_ = s.QRStale()
			s.SetQRStale(false)
		}()
	}
	wg.Wait()
	if s.QRStale() {
		t.Fatal("final state after even writes must be clean")
	}
}
