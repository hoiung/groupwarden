package ledger

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"time"

	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/store"
)

// purgeEvery is how often retention runs.
const purgeEvery = time.Hour

// Purger deletes what retention says is no longer kept. One purge serves every
// community, so each window is the longest across communities.
type Purger struct {
	Store  *store.Store
	Config *config.Holder
	// Session is the WhatsApp session store (nil: message secrets are not
	// purged, e.g. in store-only commands).
	Session *store.Session
	// AnnouncementChats lists the announcement groups, whose message secrets
	// are kept longer so replies to older announcements still decrypt.
	AnnouncementChats func() []string
	// Ready closes once AnnouncementChats can be trusted (the group list has
	// been read); message secrets are not purged before, or every
	// announcement secret would age out as an ordinary one (nil: at once).
	Ready <-chan struct{}
	Log   *slog.Logger
	Now   func() time.Time
}

// PurgeResult counts what one purge deleted.
type PurgeResult struct {
	Evidence, Files, Ledger, Reports, Outbox, Secrets int64
}

// Purge applies retention once.
func (p *Purger) Purge(ctx context.Context) (PurgeResult, error) {
	var res PurgeResult
	now := p.Now()
	r := p.Config.Current().Config.LongestRetention()
	evidenceCutoff := now.Add(-time.Duration(r.EvidenceDays) * 24 * time.Hour)
	n, files, err := p.Store.PurgeEvidence(ctx, evidenceCutoff)
	if err != nil {
		return res, err
	}
	res.Evidence = n
	for _, f := range files {
		if err := os.Remove(f); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- a path groupwarden wrote under data_dir
			p.Log.Error("could not delete an evidence attachment", "err", err)
			continue
		}
		res.Files++
	}
	logCutoff := now.AddDate(0, -r.ActionLogMonths, 0)
	if res.Ledger, err = p.Store.PurgeLedger(ctx, logCutoff); err != nil {
		return res, err
	}
	if res.Reports, err = p.Store.PurgeReports(ctx, logCutoff); err != nil {
		return res, err
	}
	if res.Outbox, err = p.Store.PurgeOutbox(ctx); err != nil {
		return res, err
	}
	if p.Session != nil && !p.ready() {
		p.Log.Info("message secrets kept until the group list is read")
	} else if p.Session != nil {
		var chats []string
		if p.AnnouncementChats != nil {
			chats = p.AnnouncementChats()
		}
		announcementCutoff := now.Add(-time.Duration(r.AnnouncementSecretDays) * 24 * time.Hour)
		if res.Secrets, err = p.Session.PurgeSecrets(ctx, evidenceCutoff, announcementCutoff, chats); err != nil {
			return res, err
		}
	}
	return res, nil
}

func (p *Purger) ready() bool {
	if p.Ready == nil {
		return true
	}
	select {
	case <-p.Ready:
		return true
	default:
		return false
	}
}

// Run purges at once and then every hour until ctx ends.
func (p *Purger) Run(ctx context.Context) {
	for {
		res, err := p.Purge(ctx)
		switch {
		case err != nil && ctx.Err() == nil:
			p.Log.Error("retention purge failed", "err", err)
		case err == nil:
			p.Log.Info("retention purge", "evidence", res.Evidence, "files", res.Files, "ledger", res.Ledger,
				"reports", res.Reports, "outbox", res.Outbox, "secrets", res.Secrets)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(purgeEvery):
		}
	}
}
