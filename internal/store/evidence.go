package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Media states of an evidence copy's attachment.
const (
	MediaNone     = "none"      // no attachment
	MediaPending  = "pending"   // to be downloaded (never delays the revoke)
	MediaSaved    = "saved"     // the file is in the evidence dir
	MediaTooLarge = "too_large" // over evidence.max_attachment_mb, declared or downloaded: type, name and size only
	MediaFailed   = "failed"    // the download failed, the file could not be saved, or Telegram refused it (error recorded)
)

// Evidence is the copy of a reported message kept on the node: the original
// fields and their normalised text, who sent it where, and the rule and config
// that decided it. Purged after retention.evidence_days.
type Evidence struct {
	ID         int64
	Chat       string
	Community  string
	Sender     string
	SenderAlt  string
	Subject    string // the member key (LID when known)
	PushName   string
	MsgID      string
	TargetID   string // the message an action targets (the original, for an edit)
	MsgTime    time.Time
	Fields     string // JSON: every sender-written field as sent, with its matching view
	Normalised string // JSON: each field's normalised text
	Rule       string
	ConfigHash string
	MediaKind  string
	MediaMime  string
	MediaName  string
	MediaSize  uint64 // the file's size once downloaded; before, what the post declared (0: unknown)
	MediaRaw   []byte // adapter-private description the download needs
	MediaState string
	MediaPath  string
	MediaError string
	CreatedAt  time.Time
}

// InsertEvidence writes e inside tx and returns its ID.
func InsertEvidence(ctx context.Context, tx *sql.Tx, e Evidence, now time.Time) (int64, error) {
	if e.MediaState == "" {
		e.MediaState = MediaNone
	}
	res, err := tx.ExecContext(ctx, `
INSERT INTO evidence (chat, community, sender, sender_alt, subject, push_name, msg_id, target_id, msg_time, fields,
	normalised, rule, config_hash, media_kind, media_mime, media_name, media_size, media_raw, media_state, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Chat, e.Community, e.Sender, e.SenderAlt, e.Subject, e.PushName, e.MsgID, e.TargetID, msOf(e.MsgTime), e.Fields,
		e.Normalised, e.Rule, e.ConfigHash, e.MediaKind, e.MediaMime, e.MediaName, int64(e.MediaSize), e.MediaRaw, // #nosec G115 -- a WhatsApp attachment size is far below 2^63
		e.MediaState, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("evidence insert: %w", err)
	}
	return res.LastInsertId()
}

const evidenceCols = `id, chat, community, sender, sender_alt, subject, push_name, msg_id, target_id, msg_time, fields,
	normalised, rule, config_hash, media_kind, media_mime, media_name, media_size, media_raw, media_state, media_path,
	media_error, created_at`

func scanEvidence(sc interface{ Scan(...any) error }) (Evidence, error) {
	var e Evidence
	var msgTime, created, size int64
	err := sc.Scan(&e.ID, &e.Chat, &e.Community, &e.Sender, &e.SenderAlt, &e.Subject, &e.PushName, &e.MsgID, &e.TargetID,
		&msgTime, &e.Fields, &e.Normalised, &e.Rule, &e.ConfigHash, &e.MediaKind, &e.MediaMime, &e.MediaName, &size,
		&e.MediaRaw, &e.MediaState, &e.MediaPath, &e.MediaError, &created)
	e.MsgTime, e.CreatedAt, e.MediaSize = msTime(msgTime), time.UnixMilli(created), uint64(max(size, 0)) // #nosec G115 -- clamped at 0 just before
	return e, err
}

// Evidence reads one copy (ok false when it was purged or forgotten).
func (s *Store) Evidence(ctx context.Context, id int64) (Evidence, bool, error) {
	e, err := scanEvidence(s.db.QueryRowContext(ctx, `SELECT `+evidenceCols+` FROM evidence WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return e, false, nil
	}
	if err != nil {
		return e, false, fmt.Errorf("read evidence: %w", err)
	}
	return e, true, nil
}

// PendingMedia lists copies whose attachment still has to be downloaded.
func (s *Store) PendingMedia(ctx context.Context, limit int) ([]Evidence, error) {
	return s.evidenceRows(ctx, `SELECT `+evidenceCols+` FROM evidence WHERE media_state = 'pending' ORDER BY id LIMIT ?`, limit)
}

// EvidenceFor lists the copies of messages sent by any of the given member keys
// or addresses.
func (s *Store) EvidenceFor(ctx context.Context, ids []string) ([]Evidence, error) {
	return s.evidenceRows(ctx, `SELECT `+evidenceCols+` FROM evidence WHERE subject IN (SELECT value FROM json_each(?1))
	OR sender IN (SELECT value FROM json_each(?1)) OR (sender_alt != '' AND sender_alt IN (SELECT value FROM json_each(?1)))
ORDER BY id`, jsonList(nonEmpty(ids)))
}

func (s *Store) evidenceRows(ctx context.Context, query string, args ...any) ([]Evidence, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read evidence: %w", err)
	}
	defer rows.Close()
	var out []Evidence
	for rows.Next() {
		e, err := scanEvidence(rows)
		if err != nil {
			return nil, fmt.Errorf("read evidence: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetMedia records the outcome of an attachment download, with the size it
// leaves known (0: unknown).
func (s *Store) SetMedia(ctx context.Context, id int64, state, path, errText string, size uint64) error {
	return s.Write(ctx, func(tx *sql.Tx) error {
		// The adapter-private download description is not needed once the
		// download is over.
		_, err := tx.ExecContext(ctx, `UPDATE evidence SET media_state = ?, media_path = ?, media_error = ?, media_size = ?,
	media_raw = NULL WHERE id = ?`, state, path, errText, int64(size), id) // #nosec G115 -- a WhatsApp attachment size is far below 2^63
		return err
	})
}

// PurgeEvidence deletes copies created before cutoff and returns the
// attachment files they kept, for the caller to delete.
func (s *Store) PurgeEvidence(ctx context.Context, cutoff time.Time) (n int64, files []string, err error) {
	err = s.Write(ctx, func(tx *sql.Tx) error {
		files, err = mediaPaths(ctx, tx, `SELECT media_path FROM evidence WHERE created_at < ? AND media_path != ''`, cutoff.UnixMilli())
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM evidence WHERE created_at < ?`, cutoff.UnixMilli())
		if err != nil {
			return err
		}
		n, _ = res.RowsAffected()
		return nil
	})
	return n, files, err
}

func mediaPaths(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}
