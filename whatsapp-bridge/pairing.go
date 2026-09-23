package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/mdp/qrterminal"
	qrcode "github.com/skip2/go-qrcode"
	"go.mau.fi/whatsmeow"
	waLog "go.mau.fi/whatsmeow/util/log"

	"whatsapp-bridge/wastate"
)

// qrPairer is the narrow whatsmeow seam for the pairing supervisor
// (whatsmeow.Client implements both methods).
type qrPairer interface {
	GetQRChannel(ctx context.Context) (<-chan whatsmeow.QRChannelItem, error)
	Connect() error
}

// runPairingSupervisor keeps an unpaired device's QR session alive for the
// whole process lifetime (eventlab W5.12a refinement): whatsmeow ends each
// attempt with a "timeout" item after exhausting its QR codes, closing the
// channel and disconnecting the client (qrchan.go:66-71). The supervisor
// marks the session stale (auth status qr_stale=true, pairing-qr 410),
// waits one backoff beat and re-arms GetQRChannel+Connect on the SAME
// client — the re-arm preconditions hold (IsConnected()==false,
// Store.ID==nil). Context cancel exits the supervisor; pairing success
// returns for good.
func runPairingSupervisor(
	ctx context.Context,
	p qrPairer,
	state *wastate.State,
	backoff time.Duration,
	log waLog.Logger,
) {
	for {
		qrChan, err := p.GetQRChannel(ctx)
		if err != nil {
			log.Errorf("pairing: GetQRChannel: %v", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}
		if err := p.Connect(); err != nil {
			log.Errorf("pairing: connect: %v", err)
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}
		timedOut := false
		for evt := range qrChan {
			switch evt.Event {
			case "code":
				fmt.Println("\nScan this QR code with your WhatsApp app:")
				qrterminal.GenerateHalfBlock(evt.Code, qrterminal.L, os.Stdout)
				if png, err := qrcode.Encode(evt.Code, qrcode.Medium, 256); err == nil {
					state.SetPairingQRPNG(png)
					// fresh code = fresh session: clear the stale flag
					state.SetQRStale(false)
				} else {
					log.Warnf("failed to encode pairing qr as png: %v", err)
				}
			case "success":
				log.Infof("pairing succeeded")
				return
			case "timeout":
				state.SetQRStale(true)
				state.ClearPairingQR()
				log.Warnf("pairing qr timeout — re-arming fresh session (backoff %s)", backoff)
				timedOut = true
			default:
				log.Debugf("pairing: qr item event %q ignored", evt.Event)
			}
		}
		// Channel closed (terminal event): after a timeout the client is
		// disconnected again and can be re-armed in place.
		if timedOut {
			if !sleepCtx(ctx, backoff) {
				return
			}
			continue
		}
		return
	}
}

// sleepCtx waits d or bails out early on context cancel (returns whether
// the full backoff was slept).
func sleepCtx(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}
