package whatsmeow

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"go.mau.fi/whatsmeow/proto/waAdv"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"

	gwstore "github.com/hoiung/groupwarden/internal/store"
)

// TestDecryptionWaitsForThePurge: the library's decryption transaction (a
// read of the signal session, then the write of the decrypted-event buffer,
// the shape of its bufferedDecrypt) on whatsmeow.db opened as Open opens it
// waits for the hourly secret purge that commits between its read and its
// write, instead of failing: a failed decryption loses the message to a
// retry request.
func TestDecryptionWaitsForThePurge(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "whatsmeow.db")
	c, err := sqlstore.New(ctx, "sqlite", sessionDSN(path), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	dev := c.NewDevice()
	jid := types.NewJID("447700900001", types.DefaultUserServer)
	dev.ID = &jid
	dev.Account = &waAdv.ADVSignedDeviceIdentity{Details: []byte{1}, AccountSignature: make([]byte, 64), AccountSignatureKey: make([]byte, 32), DeviceSignature: make([]byte, 64)}
	if err := c.PutDevice(ctx, dev); err != nil {
		t.Fatal(err)
	}
	// One stored message secret, so the purge writes.
	if err := dev.MsgSecrets.PutMessageSecret(ctx, types.NewJID("99999000000111", types.GroupServer),
		types.NewJID("99999000000444", types.HiddenUserServer), "M1", []byte{1}); err != nil {
		t.Fatal(err)
	}
	sess, err := gwstore.OpenSession(ctx, path, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	readDone := make(chan struct{})
	purgeDone := make(chan error, 1)
	go func() {
		<-readDone
		now := time.Now()
		_, err := sess.PurgeSecrets(ctx, now.Add(-time.Hour), now.Add(-time.Hour), nil)
		purgeDone <- err
	}()
	var hash [32]byte
	hash[0] = 7
	err = dev.EventBuffer.DoDecryptionTxn(ctx, func(ctx context.Context) error {
		if _, err := dev.Sessions.GetSession(ctx, "99999000000444.0"); err != nil {
			return err
		}
		close(readDone)
		// Give the purge time to commit if it could (it cannot: the
		// decryption holds the write lock from its start).
		select {
		case err := <-purgeDone:
			t.Logf("purge committed during the decryption: %v", err)
		case <-time.After(300 * time.Millisecond):
		}
		return dev.EventBuffer.PutBufferedEvent(ctx, hash, []byte("plaintext"), time.Now())
	})
	if err != nil {
		t.Fatalf("decryption failed beside the purge: %v", err)
	}
	if err := <-purgeDone; err != nil {
		t.Fatalf("purge after the decryption: %v", err)
	}
}
