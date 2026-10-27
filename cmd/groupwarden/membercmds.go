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

	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/config"
	"github.com/hoiung/groupwarden/internal/ledger"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/rules"
	"github.com/hoiung/groupwarden/internal/store"
)

// parseMember reads a member given as a LID ("<digits>@lid") or a phone
// number: a full "<digits>@s.whatsapp.net", or written with its country code
// as /ban <number> takes it (client.PhoneJID).
func parseMember(arg string) (client.Member, error) {
	s := strings.TrimSpace(arg)
	user, server, hasServer := strings.Cut(s, "@")
	if hasServer {
		if !allDigits(user) || (server != "lid" && server != "s.whatsapp.net") {
			return client.Member{}, fmt.Errorf("%q is not a LID (<digits>@lid) or a phone number", arg)
		}
		return client.MemberOf(client.JID(user + "@" + server)), nil
	}
	phone, ok := client.PhoneJID(s)
	if !ok {
		return client.Member{}, fmt.Errorf("%q is not a LID (<digits>@lid) or a phone number with its country code", arg)
	}
	return client.Member{Phone: phone}, nil
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

// ban is `ban add|remove|list`. `ban add` writes the ban on the member's
// LID, with their number, when the WhatsApp session store's saved IDs or
// groupwarden's records pair the address given with the other (banMember);
// a phone number neither pairs is the ban key until the running bot's next
// sweep looks up its LID.
func (e *env) ban(ctx context.Context, l *config.Loaded, sub string, args []string, community string) int {
	cfg := l.Config
	st, err := e.openStore(ctx, cfg)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	defer st.Close()
	// --community may name a community switched on with /enable.
	list, err := st.EnabledList(ctx)
	if err == nil {
		l, err = l.WithEnabled(app.EnabledEntries(list))
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: the /enable list: %v\n", err)
		return exitFail
	}
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
		held, err := st.HeldBans(ctx)
		if err != nil {
			fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
			return exitFail
		}
		for _, r := range held {
			fmt.Fprintf(e.stdout, "%s scope=%s held by a pause (applies once the pause ends) since=%s reason=%q\n", r.Target, r.Chat,
				r.CreatedAt.UTC().Format(time.RFC3339), r.Reason)
		}
		fmt.Fprintf(e.stdout, "%d ban(s)\n", len(bans))
		if len(held) > 0 {
			fmt.Fprintf(e.stdout, "%d ban(s) held by a pause\n", len(held))
		}
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
	perCommunity := l.Rules.BanScope() == rules.PerCommunity
	switch {
	case community != "" && !perCommunity:
		fmt.Fprintln(e.stderr, "groupwarden: bans.scope is all_communities: a ban covers every community, so leave out --community")
		return exitUsage
	case community != "":
		// A ban is added only where the bot moderates, but lifted anywhere it
		// was held: a community switched off with /disable keeps its bans.
		if _, ok := l.Rules.ModeFor(community); !ok && sub == "add" {
			fmt.Fprintf(e.stderr, "groupwarden: %s is not a configured community (nor switched on with /enable)\n", community)
			return exitUsage
		}
		scope = community
	case perCommunity && sub == "add":
		fmt.Fprintln(e.stderr, "groupwarden: bans.scope is per_community: name the community with --community <id>")
		return exitUsage
	}
	// Removing with --community lifts the ban in that community only; without
	// it, every ban of the member.
	in := ""
	if community != "" {
		in = " in " + community
	}
	// By the address given and its pair (banMember): a ban is added on their
	// LID with the number, as the sweep would resolve it, and a ban that names
	// them by the address not given (by LID alone, for someone who joined
	// since the group list was last read) is found by the number too.
	who, err := e.banMember(ctx, st, cfg, m)
	if err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	p := ledger.Plan{Trigger: store.CLITrigger + strconv.FormatInt(e.now().UnixMilli(), 10), Target: who, ConfigHash: l.Hash,
		Actor: "cli", BanCommunity: scope}
	switch sub {
	case "add":
		p.Ban, p.BanEnforce, p.Reason = []string{scope}, true, "added with the ban command"
		p.Reports = []store.Report{{Kind: ledger.KindBanCLI, Community: scope, Text: "A ban was added with the ban command."}}
	case "remove":
		// A ban a pause holds is lifted too (it would apply once the pause ends).
		_, banned, err := st.FindBan(ctx, p.Target.IDs(), community)
		if err == nil && !banned {
			banned, err = st.HeldBan(ctx, p.Target.IDs(), community)
		}
		if err != nil {
			fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
			return exitFail
		}
		if !banned {
			fmt.Fprintf(e.stdout, "%s is not banned%s\n", m.Key(), in)
			return exitFail
		}
		// By a phone number, every ban of the number goes; by a LID, a ban of
		// someone who held their number before stays on that LID (Plan.Lift).
		p.Lift, p.LiftIn, p.LiftByNumber, p.Reason = true, community, m.LID == "", "removed with the ban command"
		p.Reports = []store.Report{{Kind: ledger.KindBanCLI, Community: scope, Text: "A ban was lifted with the ban command."}}
	default:
		fmt.Fprintln(e.stderr, "usage: groupwarden ban add|remove|list [<lid|phone>]")
		return exitUsage
	}
	var w ledger.Written
	if err := st.Write(ctx, func(tx *sql.Tx) error {
		if w, err = ledger.Write(ctx, tx, p, e.now()); err != nil || sub != "add" || who.LID == "" || who.Phone == "" {
			return err
		}
		// A ban of the number already on the list (an earlier `ban add` that
		// found no LID) is merged into this one, as the sweep would merge it.
		return store.ResolveBanIn(ctx, tx, string(who.Phone), string(who.LID))
	}); err != nil {
		fmt.Fprintf(e.stderr, "groupwarden: %v\n", err)
		return exitFail
	}
	if sub == "add" {
		// Someone already on the list keeps their one entry; an admin's ban
		// of someone the bot banned is recorded on it (ledger.Written).
		if w.BansAdded == 0 {
			fmt.Fprintf(e.stdout, "already banned %s (scope %s)\n", who.Key(), scope)
			return exitOK
		}
		fmt.Fprintf(e.stdout, "banned %s (scope %s)\n", who.Key(), scope)
		if who.LID == "" {
			fmt.Fprintln(e.stdout, "the running bot looks up this number's LID at its next sweep")
		}
		return exitOK
	}
	fmt.Fprintf(e.stdout, "unbanned %s%s\n", m.Key(), in)
	if w.NumbersReleased > 0 {
		fmt.Fprintf(e.stdout, "%d ban(s) of someone who had their number before stay on that person, without the number\n",
			w.NumbersReleased)
	}
	return exitOK
}

// banMember is m with the address paired with the one given (pair: the
// saved WhatsApp IDs, then groupwarden's records; the command line has no
// group list and does not ask WhatsApp).
func (e *env) banMember(ctx context.Context, st *store.Store, cfg *config.Config, m client.Member) (client.Member, error) {
	session, err := openSession(ctx, cfg, e.now)
	if err != nil {
		return m, err
	}
	if session != nil {
		defer session.Close()
	}
	other, err := pair(ctx, st, session, m)
	if err != nil {
		return m, err
	}
	return client.MemberOf(m.LID, m.Phone, other), nil
}

// pair is the address paired with m's one address, one step from it and never
// a second: the WhatsApp library's saved pair, else the one groupwarden's
// records give (pipeline.LIDForNumber's steps for the command line, for a
// LID the same two the other way round). A number the records give a LID is
// its pair only when that number's own pair (pipeline.LIDForNumber) is the
// LID or none: otherwise someone else holds the number now (pipeline.Identify).
func pair(ctx context.Context, st *store.Store, session *store.Session, m client.Member) (client.JID, error) {
	src := pipeline.LIDSources{Records: st}
	src.Saved = func(ctx context.Context, addr client.JID) (client.JID, error) { return savedPair(ctx, session, addr) }
	if m.LID == "" {
		lid, _, err := pipeline.LIDForNumber(ctx, m.Phone, src)
		return lid, err
	}
	if phone, err := src.Saved(ctx, m.LID); err != nil || phone != "" {
		return phone, err
	}
	phone, err := pipeline.RecordedPair(ctx, st, m.LID)
	if err != nil || phone == "" {
		return phone, err
	}
	holder, _, err := pipeline.LIDForNumber(ctx, phone, src)
	if err != nil || holder != "" && holder != m.LID.Bare() {
		return "", err
	}
	return phone, nil
}

// savedPair is the address the WhatsApp session store's LID map pairs with
// addr ("" when it has none, or there is no session store): one row per LID
// and per number.
func savedPair(ctx context.Context, session *store.Session, addr client.JID) (client.JID, error) {
	if session == nil {
		return "", nil
	}
	sm, err := session.Member(ctx, []string{string(addr)})
	if err != nil {
		return "", err
	}
	for _, row := range sm.LIDMap {
		switch {
		case addr.Server() == "lid" && row["lid"] == addr.User():
			return client.JID(row["pn"] + "@s.whatsapp.net"), nil
		case addr.Server() != "lid" && row["pn"] == addr.User():
			return client.JID(row["lid"] + "@lid"), nil
		}
	}
	return "", nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// knownIDs gathers the addresses `member show|forget` act on for m, one step
// from the address given and never a second, so they reach no second person:
// the WhatsApp session store's pair of it, the other address the bot
// recorded beside it over every ban row of theirs (store.OtherAddressEver: a
// ban lifted or undone since still names them; someone who joined since the
// group list was last read is known there by their LID alone), and the
// addresses on the ban list entries that name it.
func knownIDs(ctx context.Context, st *store.Store, session *store.Session, m client.Member) ([]string, error) {
	ids := map[string]bool{}
	add := func(id string) {
		if id != "" {
			ids[id] = true
		}
	}
	for _, id := range m.IDs() {
		add(id)
	}
	saved, err := savedPair(ctx, session, client.JID(m.Key()))
	if err != nil {
		return nil, err
	}
	add(string(saved))
	other, err := st.OtherAddressEver(ctx, m.Key())
	if err != nil {
		return nil, err
	}
	add(other)
	bans, err := st.BansFor(ctx, m.IDs())
	if err != nil {
		return nil, err
	}
	for _, b := range bans {
		add(b.Member)
		add(b.LID)
		add(b.Phone)
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
	Posts     []postJSON           `json:"posts"` // the seen table's record of their posts (no text)
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

type postJSON struct {
	Chat, MsgID, Sender, At string
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
	rec := memberRecord{Addresses: ids, Bans: []banJSON{}, Ledger: []ledgerJSON{}, Evidence: []evidenceJSON{}, Posts: []postJSON{},
		Reports: []reportJSON{}}
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
			size := "size unknown"
			if ev.MediaSize > 0 {
				size = fmt.Sprintf("%d bytes", ev.MediaSize)
			}
			media = fmt.Sprintf("%s %s %q %s (%s)", ev.MediaKind, ev.MediaMime, ev.MediaName, size, ev.MediaState)
		}
		rec.Evidence = append(rec.Evidence, evidenceJSON{ev.ID, ev.Chat, ev.Community, ev.Sender, ev.PushName, ev.MsgID, ev.Rule,
			json.RawMessage(ev.Fields), media, ev.CreatedAt.UTC().Format(time.RFC3339)})
	}
	posts, err := st.SeenBy(ctx, ids)
	if err != nil {
		return fail(err)
	}
	for _, p := range posts {
		rec.Posts = append(rec.Posts, postJSON{p.Chat, p.MsgID, p.Sender, p.ServerTime.UTC().Format(time.RFC3339)})
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
	if f.HeldBans > 0 {
		fmt.Fprintf(e.stdout, "kept: %d ban(s) a pause holds: they apply once the pause ends, like an active ban; "+
			"lift them with `groupwarden ban remove` first if they should go\n", f.HeldBans)
	}
	return exitOK
}
