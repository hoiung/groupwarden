package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/rules"
	"github.com/hoiung/groupwarden/internal/store"
)

// parseMember reads a member given as a LID ("<digits>@lid") or a phone
// number (digits with country code, spaces, + and - allowed, or a full
// "<digits>@s.whatsapp.net").
func parseMember(arg string) (client.Member, error) {
	s := strings.TrimSpace(arg)
	user, server, hasServer := strings.Cut(s, "@")
	if hasServer {
		if !allDigits(user) || (server != "lid" && server != "s.whatsapp.net") {
			return client.Member{}, fmt.Errorf("%q is not a LID (<digits>@lid) or a phone number", arg)
		}
		return client.MemberOf(client.JID(user + "@" + server)), nil
	}
	digits := strings.NewReplacer(" ", "", "+", "", "-", "", "(", "", ")", "").Replace(s)
	if !allDigits(digits) || len(digits) < 7 || len(digits) > 15 {
		return client.Member{}, fmt.Errorf("%q is not a LID (<digits>@lid) or a phone number with its country code", arg)
	}
	return client.Member{Phone: client.JID(digits + "@s.whatsapp.net")}, nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// openStore opens groupwarden.db for a store command (no lock: it works
// beside run through SQLite's own locking).
func (e *env) openStore(ctx context.Context, cfg *config.Config) (*store.Store, error) {
	return store.Open(ctx, cfg.StoreDB(), store.Options{Now: e.now})
}

// openSession opens the WhatsApp session store when it exists (nil, nil
// when the bot was never paired on this data dir).
func openSession(ctx context.Context, cfg *config.Config, now func() time.Time) (*store.Session, error) {
	if _, err := os.Stat(cfg.WhatsmeowDB()); errors.Is(err, os.ErrNotExist) { // #nosec G703 -- operator-configured data dir
		return nil, nil
	}
	return store.OpenSession(ctx, cfg.WhatsmeowDB(), now)
}

// ledgerSummary prints, per community, the count of shadow and enforce rows
// by action.
func (e *env) ledgerSummary(ctx context.Context, cfg *config.Config) int {
	st, err := e.openStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer st.Close()
	rows, err := st.LedgerSummary(ctx)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	if len(rows) == 0 {
		fmt.Fprintln(e.stdout, "no actions recorded")
		return exitOK
	}
	type key struct{ community, mode string }
	var order []key
	counts := map[key][]string{}
	for _, r := range rows {
		k := key{r.Community, r.Mode}
		if _, ok := counts[k]; !ok {
			order = append(order, k)
		}
		counts[k] = append(counts[k], fmt.Sprintf("%s=%d", r.Action, r.Count))
	}
	for _, k := range order {
		fmt.Fprintf(e.stdout, "%s %s %s\n", k.community, k.mode, strings.Join(counts[k], " "))
	}
	return exitOK
}

// ban is `ban add|remove|list`. A phone number given to `ban add` is stored
// as the ban key until the running bot resolves it to a LID at its next sweep.
func (e *env) ban(ctx context.Context, l *config.Loaded, sub string, args []string, community string) int {
	cfg := l.Config
	st, err := e.openStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer st.Close()
	if sub == "list" {
		bans, err := st.Bans(ctx)
		if err != nil {
			fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
			return exitFail
		}
		for _, b := range bans {
			fmt.Fprintf(e.stdout, "%s scope=%s lid=%s phone=%s since=%s reason=%q\n", b.Member, b.Scope, dash(b.LID),
				dash(b.Phone), b.CreatedAt.UTC().Format(time.RFC3339), b.Reason)
		}
		fmt.Fprintf(e.stdout, "%d ban(s)\n", len(bans))
		return exitOK
	}
	if len(args) != 1 {
		fmt.Fprintf(e.stderr, "usage: groupwarden ban %s <lid|phone> [--community <id>]\n", sub)
		return exitUsage
	}
	m, err := parseMember(args[0])
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitUsage
	}
	scope := store.BanEverywhere
	if community != "" {
		if _, ok := l.Rules.ModeFor(community); !ok {
			fmt.Fprintf(e.stderr, "groupwarden: %s is not a configured community\n", community)
			return exitUsage
		}
		scope = community
	} else if l.Rules.BanScope() == rules.PerCommunity && sub == "add" {
		fmt.Fprintln(e.stderr, "groupwarden: bans.scope is per_community: name the community with --community <id>")
		return exitUsage
	}
	p := ledger.Plan{Trigger: "cli:" + strconv.FormatInt(e.now().UnixMilli(), 10), Target: m, ConfigHash: l.Hash,
		Actor: "cli", BanCommunity: scope}
	switch sub {
	case "add":
		p.Ban, p.BanEnforce, p.Reason = []string{scope}, true, "added with the ban command"
		p.Reports = []store.Report{{Kind: ledger.KindBanCLI, Community: scope, Text: "A ban was added with the ban command."}}
	case "remove":
		if _, banned, err := st.FindBan(ctx, m.IDs(), ""); err != nil || !banned {
			if err != nil {
				fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
				return exitFail
			}
			fmt.Fprintf(e.stdout, "%s is not banned\n", m.Key())
			return exitFail
		}
		p.Lift, p.Reason = true, "removed with the ban command"
		p.Reports = []store.Report{{Kind: ledger.KindBanCLI, Community: scope, Text: "A ban was lifted with the ban command."}}
	default:
		fmt.Fprintln(e.stderr, "usage: groupwarden ban add|remove|list [<lid|phone>]")
		return exitUsage
	}
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		_, err := ledger.Write(ctx, tx, p, e.now())
		return err
	}); err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	if sub == "add" {
		fmt.Fprintf(e.stdout, "banned %s (scope %s)\n", m.Key(), scope)
		if m.LID == "" {
			fmt.Fprintln(e.stdout, "the running bot looks up this number's LID at its next sweep")
		}
		return exitOK
	}
	fmt.Fprintf(e.stdout, "unbanned %s\n", m.Key())
	return exitOK
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// knownIDs gathers every address the stores know for m: the ban list and the
// WhatsApp session store's LID-to-phone map.
func knownIDs(ctx context.Context, st *store.Store, session *store.Session, m client.Member) ([]string, error) {
	ids := map[string]bool{}
	for _, id := range m.IDs() {
		ids[id] = true
	}
	bans, err := st.BansFor(ctx, m.IDs())
	if err != nil {
		return nil, err
	}
	for _, b := range bans {
		for _, id := range []string{b.Member, b.LID, b.Phone} {
			if id != "" {
				ids[id] = true
			}
		}
	}
	if session != nil {
		sm, err := session.Member(ctx, keys(ids))
		if err != nil {
			return nil, err
		}
		for _, row := range sm.LIDMap {
			ids[row["lid"]+"@lid"] = true
			ids[row["pn"]+"@s.whatsapp.net"] = true
		}
	}
	return keys(ids), nil
}

func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// memberRecord is what `member show` prints.
type memberRecord struct {
	Addresses []string             `json:"addresses"`
	Bans      []banJSON            `json:"bans"`
	Ledger    []ledgerJSON         `json:"ledger"`
	Evidence  []evidenceJSON       `json:"evidence"`
	Session   *store.SessionMember `json:"whatsmeow,omitempty"`
	Reports   []reportJSON         `json:"reports"`
}

type banJSON struct {
	Member, Scope, LID, Phone, Reason, Since string
}

type ledgerJSON struct {
	ID                                                         int64
	Action, Chat, Community, Mode, Status, Reason, Rule, Actor string
	Trigger, ConfigHash, At                                    string
}

type evidenceJSON struct {
	ID                                             int64
	Chat, Community, Sender, PushName, MsgID, Rule string
	Fields                                         json.RawMessage
	Media                                          string
	At                                             string
}

type reportJSON struct {
	ID         int64
	Kind, Text string
	At         string
}

// member is `member show|forget`.
func (e *env) member(ctx context.Context, cfg *config.Config, sub string, args []string) int {
	if len(args) != 1 || (sub != "show" && sub != "forget") {
		fmt.Fprintln(e.stderr, "usage: groupwarden member show|forget <lid|phone>")
		return exitUsage
	}
	m, err := parseMember(args[0])
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitUsage
	}
	st, err := e.openStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer st.Close()
	session, err := openSession(ctx, cfg, e.now)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	if session != nil {
		defer session.Close()
	}
	ids, err := knownIDs(ctx, st, session, m)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	if sub == "show" {
		return e.memberShow(ctx, st, session, ids)
	}
	return e.memberForget(ctx, st, session, ids)
}

func (e *env) memberShow(ctx context.Context, st *store.Store, session *store.Session, ids []string) int {
	rec := memberRecord{Addresses: ids, Bans: []banJSON{}, Ledger: []ledgerJSON{}, Evidence: []evidenceJSON{}, Reports: []reportJSON{}}
	fail := func(err error) int {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	bans, err := st.BansFor(ctx, ids)
	if err != nil {
		return fail(err)
	}
	for _, b := range bans {
		rec.Bans = append(rec.Bans, banJSON{b.Member, b.Scope, b.LID, b.Phone, b.Reason, b.CreatedAt.UTC().Format(time.RFC3339)})
	}
	rows, err := st.LedgerForTargets(ctx, ids)
	if err != nil {
		return fail(err)
	}
	for _, r := range rows {
		rec.Ledger = append(rec.Ledger, ledgerJSON{r.ID, string(r.Action), r.Chat, r.Community, r.Mode, string(r.Status), r.Reason,
			r.Rule, r.Actor, r.TriggerID, r.ConfigHash, r.CreatedAt.UTC().Format(time.RFC3339)})
	}
	evs, err := st.EvidenceFor(ctx, ids)
	if err != nil {
		return fail(err)
	}
	for _, ev := range evs {
		media := ev.MediaState
		if ev.MediaKind != "" {
			media = fmt.Sprintf("%s %s %q %d bytes (%s)", ev.MediaKind, ev.MediaMime, ev.MediaName, ev.MediaSize, ev.MediaState)
		}
		rec.Evidence = append(rec.Evidence, evidenceJSON{ev.ID, ev.Chat, ev.Community, ev.Sender, ev.PushName, ev.MsgID, ev.Rule,
			json.RawMessage(ev.Fields), media, ev.CreatedAt.UTC().Format(time.RFC3339)})
	}
	reps, err := st.ReportsFor(ctx, ids)
	if err != nil {
		return fail(err)
	}
	for _, r := range reps {
		rec.Reports = append(rec.Reports, reportJSON{r.ID, r.Kind, r.Text, r.CreatedAt.UTC().Format(time.RFC3339)})
	}
	if session != nil {
		sm, err := session.Member(ctx, ids)
		if err != nil {
			return fail(err)
		}
		rec.Session = &sm
	}
	enc := json.NewEncoder(e.stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		return fail(err)
	}
	return exitOK
}

func (e *env) memberForget(ctx context.Context, st *store.Store, session *store.Session, ids []string) int {
	f, err := st.ForgetMember(ctx, ids)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	files := 0
	for _, p := range f.Files {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) { // #nosec G703 -- a path groupwarden wrote under data_dir
			fmt.Fprintf(e.stderr, "groupwarden: could not delete %s: %v\n", p, err)
			continue
		}
		files++
	}
	var sessionRows int64
	if session != nil {
		if sessionRows, err = session.ForgetMember(ctx, ids); err != nil {
			fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
			return exitFail
		}
	}
	fmt.Fprintf(e.stdout, "deleted: %d evidence copies (%d attachment files), %d action-log rows, %d queued messages, %d WhatsApp contact/LID rows\n",
		f.Evidence, files, f.Ledger, f.Inbox, sessionRows)
	fmt.Fprintf(e.stdout, "queued: text removal and attachment deletion for %d admin-chat report(s)\n", f.ReportEdits)
	for _, b := range f.KeptBans {
		fmt.Fprintf(e.stdout, "kept: ban of %s (scope %s) since %s: an active ban is kept so a removed spammer cannot rejoin; "+
			"lift it with `groupwarden ban remove %s` first if it should go\n", b.Member, b.Scope,
			b.CreatedAt.UTC().Format("2006-01-02"), b.Member)
	}
	return exitOK
}
