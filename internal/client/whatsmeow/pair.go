package whatsmeow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/mdp/qrterminal/v3"
	wm "go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/hoiung/groupwarden/internal/mask"
)

// firstLoginWait is how long Pair waits, after the phone accepted the
// pairing, for the new device's first login.
const firstLoginWait = 90 * time.Second

// Pair links this device to the bot phone: a QR code to scan, or, when phone
// is given (digits with country code), an 8-character code to type into
// WhatsApp > Linked devices > Link with phone number. WhatsApp allows about
// 160 seconds for either.
func (a *Adapter) Pair(ctx context.Context, phone string, out io.Writer) error {
	if a.real == nil {
		return errors.New("pairing needs the real WhatsApp client")
	}
	if a.Paired() {
		p, _ := a.cli.ownIDs()
		return fmt.Errorf("already paired as %s: unlink that device on the bot phone and delete whatsmeow.db first", mask.IDs(p.User))
	}
	// WhatsApp ends a pairing with a stream error 515: the library's own
	// reconnect then makes the new device's first login. The phone shows the
	// device as linked only after that login, so Pair waits for it.
	a.real.DisableLoginAutoReconnect = false
	loggedIn := make(chan struct{}, 1)
	id := a.real.AddEventHandler(func(evt any) {
		if _, ok := evt.(*events.Connected); ok {
			select {
			case loggedIn <- struct{}{}:
			default:
			}
		}
	})
	defer a.real.RemoveEventHandler(id)
	qr, err := a.real.GetQRChannel(ctx)
	if err != nil {
		return fmt.Errorf("start pairing: %w", err)
	}
	if err := a.real.ConnectContext(ctx); err != nil {
		return fmt.Errorf("connect for pairing: %w", err)
	}
	codeShown := false
	for {
		var item wm.QRChannelItem
		select {
		case <-ctx.Done():
			return fmt.Errorf("pairing stopped: %w", ctx.Err())
		case next, ok := <-qr:
			if !ok {
				return errors.New("pairing ended without success")
			}
			item = next
		}
		switch item.Event {
		case "code":
			if phone == "" {
				fmt.Fprintln(out, "Scan this with the bot phone: WhatsApp > Linked devices > Link a device")
				qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, out)
				continue
			}
			if codeShown {
				continue
			}
			code, err := a.real.PairPhone(ctx, phone, true, wm.PairClientChrome, "Chrome (Linux)")
			if err != nil {
				return fmt.Errorf("request pairing code: %w", err)
			}
			codeShown = true
			fmt.Fprintf(out, "On the bot phone: WhatsApp > Linked devices > Link with phone number, then type: %s\n", code)
		case wm.QRChannelSuccess.Event:
			fmt.Fprintln(out, "Code accepted; waiting for the first login...")
			select {
			case <-loggedIn:
				fmt.Fprintln(out, "Paired.")
				return nil
			case <-ctx.Done():
				return fmt.Errorf("stopped before the first login: %w", ctx.Err())
			case <-time.After(firstLoginWait):
				return errors.New("the first login did not finish: unlink the device on the bot phone if it is listed, move whatsmeow.db aside and pair again")
			}
		case wm.QRChannelTimeout.Event:
			return errors.New("pairing timed out: run `groupwarden pair` again")
		default:
			if item.Error != nil {
				return fmt.Errorf("pairing failed (%s): %w", item.Event, item.Error)
			}
			return fmt.Errorf("pairing failed: %s", item.Event)
		}
	}
}
