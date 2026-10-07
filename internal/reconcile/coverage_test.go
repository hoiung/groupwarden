package reconcile_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hoiung/groupwarden/internal/action"
	"github.com/hoiung/groupwarden/internal/alert"
	"github.com/hoiung/groupwarden/internal/app"
	"github.com/hoiung/groupwarden/internal/client"
	"github.com/hoiung/groupwarden/internal/client/clienttest"
	"github.com/hoiung/groupwarden/internal/modtest"
	"github.com/hoiung/groupwarden/internal/pipeline"
	"github.com/hoiung/groupwarden/internal/reconcile"
	"github.com/hoiung/groupwarden/internal/store"
)

// Linked groups of the kit's community beyond G1 and G2 (synthetic IDs from
// .secret-pii-allowlist): the bot is in neither.
const (
	G3  client.JID = "99999000000333@g.us" // "events"
	Ann client.JID = "99999000000777@g.us" // the announcement group
)

// sweeper is a sweep over a kit.
type sweeper struct {
	*reconcile.Sweep
	k *modtest.Kit
	n int
}

func newSweep(k *modtest.Kit) *sweeper {
	return &sweeper{k: k, Sweep: &reconcile.Sweep{Enforcer: k.Enforcer, Directory: k.Dir, Config: k.Holder, Log: k.Log,
		Sleep: k.Clock.Sleep, Now: k.Clock.Now}}
}

// run sweeps once and fails the test on an error.
func (s *sweeper) run(t *testing.T) reconcile.Result {
	t.Helper()
	s.n++
	res, err := s.Run(s.k.Ctx, fmt.Sprintf("run%d", s.n))
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// group returns the coverage of jid in the last sweep's listing.
func group(t *testing.T, cov reconcile.Coverage, jid client.JID) reconcile.GroupCoverage {
	t.Helper()
	for _, cc := range cov.Communities {
		for _, g := range cc.Groups {
			if g.JID == jid {
				return g
			}
		}
	}
	t.Fatalf("%s not in the coverage %+v", jid, cov)
	return reconcile.GroupCoverage{}
}

// texts lists the text of every stored report of kind.
func texts(k *modtest.Kit, kind alert.Kind) []string {
	var out []string
	for _, r := range k.Reports(string(kind)) {
		out = append(out, r.Text)
	}
	return out
}

// mustContain fails unless text contains every part.
func mustContain(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(text, p) {
			t.Fatalf("%q lacks %q", text, p)
		}
	}
}

// waitFor polls cond (the app runs in goroutines of its own).
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// startApp runs `run`'s supervisor over the kit and returns a function that
// stops it. With exec nil, tests fire the outbox themselves.
func startApp(t *testing.T, k *modtest.Kit, s *sweeper, exec *action.Executor) func() {
	t.Helper()
	a := &app.App{Adapter: k.Fake, Store: k.Store, Inbox: k.Worker.Inbox, Worker: k.Worker, Alerter: k.Alerts,
		Log: k.Log, Clock: k.Clock, Config: k.Holder, Directory: k.Dir, Sweep: s.Sweep, Executor: exec,
		Settings: app.Settings{DeafAfter: 24 * time.Hour, DisconnectAlert: 24 * time.Hour, CompanionCheckEvery: time.Hour}}
	ctx, cancel := context.WithCancel(k.Ctx)
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	return func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("run: %v", err)
		}
	}
}

// The supervisor's log lines for a sweep that waits: a test that checks no
// sweep ran waits for one of these first, so the check does not depend on
// how fast the machine is.
const (
	waitingForBacklog = "sweep waiting for WhatsApp to deliver the offline backlog"
	waitingForWorker  = "sweep waiting for the worker to decide the offline backlog"
)

// sweeps counts the sweeps that reached G1's join requests.
func sweeps(k *modtest.Kit) int { return k.Fake.Count("JoinRequests " + string(modtest.G1)) }

// TestCoverageAbsentNotAdminCovered: every linked group of a community (the
// announcement group included) and every group of a standalone set is listed
// with where the bot stands, absent / present-not-admin / covered, and its
// human admins; the first sweep tells the admins in one list per community;
// a demotion shows at once, before the next sweep.
func TestCoverageAbsentNotAdminCovered(t *testing.T) {
	k := modtest.New(t, "")
	groups := modtest.Groups()
	groups[2].Participants[0].IsAdmin = false // the bot is in "jobs" but not an admin
	k.Fake.SetGroups(groups)
	modtest.Load(t, k.Dir, groups)
	k.Fake.SetLinked(modtest.Community, []client.GroupRef{{JID: Ann, Name: "announcements", IsAnnouncement: true},
		{JID: modtest.G1, Name: "general"}, {JID: modtest.G2, Name: "jobs"}, {JID: G3, Name: "events"}})
	s := newSweep(k)
	s.run(t)
	cov := s.Coverage()
	if !cov.At.Equal(modtest.T0) || len(cov.Communities) != 2 {
		t.Fatalf("coverage %+v", cov)
	}
	want := map[client.JID]reconcile.GroupCoverage{
		Ann:        {JID: Ann, Name: "announcements", Announcement: true, State: store.CoverageAbsent},
		modtest.G1: {JID: modtest.G1, Name: "general", State: store.CoverageCovered, HumanAdmins: 1},
		modtest.G2: {JID: modtest.G2, Name: "jobs", State: store.CoverageNotAdmin, HumanAdmins: 1},
		G3:         {JID: G3, Name: "events", State: store.CoverageAbsent},
		modtest.GB: {JID: modtest.GB, Name: "standalone", State: store.CoverageCovered, HumanAdmins: 1},
	}
	for jid, w := range want {
		if g := group(t, cov, jid); g != w {
			t.Fatalf("%s: %+v, want %+v", jid, g, w)
		}
	}
	lists := texts(k, alert.Coverage)
	if len(lists) != 2 {
		t.Fatalf("lists %q, want one per community", lists)
	}
	mustContain(t, lists[0], "The bot checked the 4 group(s) of community a:", "general (group…0111): covered",
		"jobs (group…0222): the bot is in it but not an admin: a human admin must promote it",
		"events (group…0333): the bot joined it by itself: a human admin must promote it")
	mustContain(t, lists[1], "The bot checked the 1 group(s) of set b:", "standalone (group…0888): covered")
	rows, err := k.Store.CoverageRows(k.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := rows[string(modtest.G2)]; r.State != store.CoverageNotAdmin || !r.Listed || r.Community != string(modtest.Community) {
		t.Fatalf("stored jobs row %+v", r)
	}
	// A demotion shows at once.
	k.Deliver(&client.GroupChange{Group: modtest.G1, Actor: modtest.Admin, Time: k.Clock.Now(), Demoted: []client.JID{modtest.Bot}})
	if g := group(t, s.Coverage(), modtest.G1); g.State != store.CoverageNotAdmin {
		t.Fatalf("general after the demotion: %+v", g)
	}
}

// TestFewerThanTwoHumanAdminsAlert: a group the bot is in with fewer than 2
// human admins draws one routine alert per episode; a group with 2 does not,
// and neither does a group the bot is not in.
func TestFewerThanTwoHumanAdminsAlert(t *testing.T) {
	k := modtest.New(t, "")
	groups := modtest.Groups()
	groups[1].Participants[3].IsAdmin = true // "general" has two human admins
	k.Fake.SetGroups(groups)
	modtest.Load(t, k.Dir, groups)
	k.Fake.SetLinked(modtest.Community, []client.GroupRef{{JID: modtest.G1, Name: "general"},
		{JID: modtest.G2, Name: "jobs"}, {JID: G3, Name: "events"}})
	s := newSweep(k)
	s.run(t)
	few := texts(k, alert.FewHumanAdmins)
	if len(few) != 2 {
		t.Fatalf("alerts %q, want jobs and standalone", few)
	}
	mustContain(t, few[0], "Only 1 human admin(s) in jobs (group…0222) (community a): keep at least 2")
	mustContain(t, few[1], "Only 1 human admin(s) in standalone (group…0888) (set b)")
	for _, r := range k.Reports(string(alert.FewHumanAdmins)) {
		if r.Priority {
			t.Fatalf("a few-admins alert is routine: %+v", r)
		}
	}
	s.run(t)
	if n := len(texts(k, alert.FewHumanAdmins)); n != 2 {
		t.Fatalf("%d alerts after a second sweep, want still 2", n)
	}
	// "jobs" gets a second admin (the episode ends), then loses one again.
	k.Deliver(&client.GroupChange{Group: modtest.G2, Actor: modtest.Admin, Time: k.Clock.Now(),
		Joined: []client.JID{modtest.Member}, Promoted: []client.JID{modtest.Member}})
	s.run(t)
	if n := len(texts(k, alert.FewHumanAdmins)); n != 2 {
		t.Fatalf("%d alerts with 2 human admins, want still 2", n)
	}
	k.Deliver(&client.GroupChange{Group: modtest.G2, Actor: modtest.Admin, Time: k.Clock.Now(),
		Demoted: []client.JID{modtest.Member}})
	s.run(t)
	if few := texts(k, alert.FewHumanAdmins); len(few) != 3 || !strings.Contains(few[2], "jobs") {
		t.Fatalf("alerts %q, want a new one for jobs", few)
	}
}

// TestSweepRemovesBannedPresent: a banned member the sweep finds in a
// moderated group is removed there, and a banned member's pending join
// request is rejected.
func TestSweepRemovesBannedPresent(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.SpammerM)
	k.Ban(modtest.Other1M)
	k.Fake.Requests = map[client.JID][]client.JoinRequest{modtest.G2: {{JID: modtest.Other1}}}
	s := newSweep(k)
	s.run(t)
	k.Fire()
	for _, call := range []string{"Remove " + string(modtest.G1) + " ", "Remove " + string(modtest.G2) + " ",
		"RejectJoinRequests " + string(modtest.G2) + " " + string(modtest.Other1)} {
		if k.Fake.Count(call) != 1 {
			t.Fatalf("%q sent %d times: %v", call, k.Fake.Count(call), k.Fake.Calls())
		}
	}
	if len(k.Find(modtest.SpammerM, store.ActRemove, modtest.G1)) != 1 || len(k.Find(modtest.SpammerM, store.ActRemove, modtest.G2)) != 1 {
		t.Fatalf("ledger rows %+v", k.Rows(modtest.SpammerM))
	}
	// Nobody else was touched (the member and the admin are not banned).
	if n := k.Fake.Count("Remove "); n != 2 {
		t.Fatalf("%d removals: %v", n, k.Fake.Calls())
	}
}

// TestSweepSkipsHumanReadd: a banned member a human admin re-added has had
// the ban lifted, so the sweep leaves them; one who joined by an invite link
// is removed.
func TestSweepSkipsHumanReadd(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	k.Deliver(&client.GroupChange{Group: modtest.G1, Actor: modtest.Admin, Time: k.Clock.Now(), Joined: []client.JID{modtest.Other1}})
	if k.Banned(modtest.Other1M, "") || len(k.Reports("ban_lifted")) != 1 {
		t.Fatalf("the human re-add did not lift the ban: banned=%v", k.Banned(modtest.Other1M, ""))
	}
	k.Ban(modtest.Other2M)
	k.Dir.Apply(&client.GroupChange{Group: modtest.G1, JoinReason: "invite", Joined: []client.JID{modtest.Other2}})
	s := newSweep(k)
	s.run(t)
	k.Fire()
	if n := k.Fake.Count("Remove " + string(modtest.G1) + " " + string(modtest.Other1)); n != 0 {
		t.Fatalf("the re-added member was removed: %v", k.Fake.Calls())
	}
	if n := k.Fake.Count("Remove " + string(modtest.G1) + " " + string(modtest.Other2)); n != 1 {
		t.Fatalf("the banned member who joined by link was not removed: %v", k.Fake.Calls())
	}
}

// TestSweepDetectsNewGroup: a group that appears in a community after its
// first list is reported once, with what an admin must do.
func TestSweepDetectsNewGroup(t *testing.T) {
	k := modtest.New(t, "")
	s := newSweep(k)
	s.run(t)
	if n := len(texts(k, alert.Coverage)); n != 2 {
		t.Fatalf("%d lists, want one per community", n)
	}
	// A human admin adds the bot to a new linked group; it is not an admin yet.
	groups := append(modtest.Groups(), client.Group{JID: G3, Name: "events", Parent: modtest.Community,
		Participants: []client.Participant{{JID: modtest.Bot, LID: modtest.Bot, Phone: modtest.BotPhone},
			{JID: modtest.Admin, LID: modtest.Admin, Phone: modtest.AdminPhone, IsAdmin: true}}})
	k.Fake.SetGroups(groups)
	modtest.Load(t, k.Dir, groups)
	s.run(t)
	reps := k.Reports(string(alert.Coverage))
	if len(reps) != 3 {
		t.Fatalf("reports %+v, want one more for the new group", reps)
	}
	mustContain(t, reps[2].Text, "New group in community a: events (group…0333): the bot is in it but not an admin: "+
		"a human admin must promote it.")
	if reps[2].Priority || reps[2].Community != string(modtest.Community) {
		t.Fatalf("new-group report %+v", reps[2])
	}
	if n := k.Fake.Count("JoinLinkedGroup"); n != 0 {
		t.Fatalf("joined a group the bot is already in: %v", k.Fake.Calls())
	}
	s.run(t)
	if n := len(texts(k, alert.Coverage)); n != 3 {
		t.Fatalf("%d reports after another sweep, want still 3", n)
	}
	// WhatsApp failing to list the community keeps what the admins were told:
	// the next good list reports nothing new.
	k.Fake.FailNext("SubGroups", errors.New("connection reset"))
	s.run(t)
	if s.Coverage().Communities[0].Err == "" {
		t.Fatalf("listing error not shown: %+v", s.Coverage())
	}
	s.run(t)
	if n := len(texts(k, alert.Coverage)); n != 3 {
		t.Fatalf("%d reports after a failed listing, want still 3", n)
	}
	// A group unlinked from the community is forgotten.
	k.Fake.SetGroups(modtest.Groups())
	modtest.Load(t, k.Dir, modtest.Groups())
	s.run(t)
	rows, err := k.Store.CoverageRows(k.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := rows[string(G3)]; ok || len(rows) != 3 {
		t.Fatalf("stored rows %+v, want general, jobs and standalone", rows)
	}
}

// TestSweepAfterRepair: the sweep runs when `run` connects, again after a
// reconnect, and again when `run` starts over the same database after the
// bot phone was paired again; a member banned meanwhile is removed then.
func TestSweepAfterRepair(t *testing.T) {
	k := modtest.New(t, "")
	s := newSweep(k)
	stop := startApp(t, k, s, nil)
	waitFor(t, "the sweep at connect", func() bool { return sweeps(k) == 1 })
	begun := k.Fake.Count("SubGroups")
	// The connection drops and the supervisor reconnects after its backoff
	// (the library's own reconnect is off), as in production: this time
	// WhatsApp says Connected but has not yet delivered what it held.
	k.Fake.OnConnect = func(f *clienttest.Fake) { f.Emit(client.Lifecycle{Kind: client.Connected}) }
	waits := k.Logged(waitingForBacklog)
	k.Fake.Emit(client.Lifecycle{Kind: client.Disconnected})
	waitFor(t, "the reconnect scheduled", func() bool { return k.Logged("disconnected; reconnecting") == 1 })
	k.Clock.Advance(5 * time.Minute) // the longest backoff
	// The sweep after the reconnect waits for WhatsApp to deliver what it
	// held meanwhile, as the first one did.
	waitFor(t, "the sweep after the reconnect waiting", func() bool { return k.Logged(waitingForBacklog) == waits+1 })
	if n := k.Fake.Count("SubGroups"); n != begun {
		t.Fatalf("a sweep began before WhatsApp delivered the offline backlog after the reconnect (%d, was %d)", n, begun)
	}
	k.Fake.Emit(client.Lifecycle{Kind: client.CaughtUp})
	waitFor(t, "the sweep after a reconnect", func() bool { return sweeps(k) == 2 })
	stop()
	k.Fake.OnConnect = nil // the next start connects with nothing held
	// `groupwarden pair`, then `run` again: a member banned while the bot was
	// down is in "general".
	k.Ban(modtest.Other1M)
	groups := modtest.Groups()
	groups[1].Participants = append(groups[1].Participants, client.Participant{JID: modtest.Other1, LID: modtest.Other1})
	k.Fake.SetGroups(groups)
	stop = startApp(t, k, newSweep(k), nil)
	waitFor(t, "the sweep after re-pairing", func() bool { return sweeps(k) == 3 })
	stop()
	k.Fire()
	if n := k.Fake.Count("Remove " + string(modtest.G1) + " " + string(modtest.Other1)); n != 1 {
		t.Fatalf("the banned member was not removed after re-pairing: %v", k.Fake.Calls())
	}
}

// freshDirectory gives the kit an empty group directory, as `run` has when
// it starts: nothing is decided until a group list loads.
func freshDirectory(k *modtest.Kit) {
	d := &pipeline.Directory{}
	d.SetSelf(k.Fake.SelfIDs)
	k.Dir, k.Enforcer.Directory, k.Mod.Directory, k.Exec.Directory = d, d, d, d
}

// TestConnectSweepWaitsForOfflineBacklog: after a connect the sweep waits
// until WhatsApp has said it delivered what it held while the bot was
// offline and the worker has decided all of it. A human admin re-added a
// banned member meanwhile: the re-add lifts the ban before the sweep looks,
// so the sweep does not remove them (and the admins are not told "the ban is
// lifted" about someone already removed).
func TestConnectSweepWaitsForOfflineBacklog(t *testing.T) {
	k := modtest.New(t, "")
	k.Ban(modtest.Other1M)
	groups := modtest.Groups()
	groups[1].Participants = append(groups[1].Participants, client.Participant{JID: modtest.Other1, LID: modtest.Other1})
	k.Fake.SetGroups(groups)
	k.Fake.OnConnect = func(f *clienttest.Fake) { f.Emit(client.Lifecycle{Kind: client.Connected}) }
	// The worker cannot record its decisions yet: it is behind on the backlog.
	lift := modtest.FailWrites(t, k.Store, "seen", "")
	stop := startApp(t, k, newSweep(k), nil)
	waitFor(t, "the list at connect", func() bool { return k.Fake.Count("JoinedGroups") == 1 })
	readd := &client.GroupChange{Group: modtest.G1, Actor: modtest.Admin, Time: k.Clock.Now(),
		Joined: []client.JID{modtest.Other1}}
	if err := k.Fake.Deliver(readd); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the sweep waiting for the backlog", func() bool { return k.Logged(waitingForBacklog) == 1 })
	if n := k.Fake.Count("SubGroups"); n != 0 {
		t.Fatalf("the sweep began before WhatsApp delivered the offline backlog: %v", k.Fake.Calls())
	}
	k.Fake.Emit(client.Lifecycle{Kind: client.CaughtUp, Detail: "1 offline events"})
	waitFor(t, "the sweep waiting for the worker", func() bool { return k.Logged(waitingForWorker) == 1 })
	for range 3 { // the sweep checks the inbox every second
		time.Sleep(20 * time.Millisecond)
		k.Clock.Advance(time.Second)
	}
	time.Sleep(50 * time.Millisecond)
	if n := k.Fake.Count("SubGroups"); n != 0 {
		t.Fatalf("the sweep began before the worker decided the offline backlog: %v", k.Fake.Calls())
	}
	lift()
	// A new message wakes the worker, which decides the re-add first.
	if err := k.Fake.Deliver(k.Msg("M1", modtest.G1, modtest.Member, "hello")); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the sweep", func() bool {
		k.Clock.Advance(time.Second)
		return sweeps(k) >= 1
	})
	stop()
	k.Fire()
	if n := k.Fake.Count("Remove " + string(modtest.G1) + " " + string(modtest.Other1)); n != 0 {
		t.Fatalf("the member a human admin re-added was removed: %v", k.Fake.Calls())
	}
	if k.Banned(modtest.Other1M, "") || len(k.Reports("ban_lifted")) != 1 {
		t.Fatalf("the re-add did not lift the ban: banned=%v, reports %d", k.Banned(modtest.Other1M, ""),
			len(k.Reports("ban_lifted")))
	}
}

// TestSweepWaitsForTheGroupList: a sweep never runs before the group list
// has loaded. Against an empty directory every linked group looks like one
// the bot is not in: it would try to join them all and report them lost.
func TestSweepWaitsForTheGroupList(t *testing.T) {
	k := modtest.New(t, "")
	freshDirectory(k)
	k.Fake.FailNext("JoinedGroups", errors.New("rate limited"))
	stop := startApp(t, k, newSweep(k), nil)
	defer stop()
	waitFor(t, "the failed list at connect", func() bool { return k.Fake.Count("JoinedGroups") == 1 })
	waitFor(t, "the sweep waiting for the list", func() bool { return k.Logged("sweep waiting for the group list") == 1 })
	if n := k.Fake.Count("SubGroups") + k.Fake.Count("JoinLinkedGroup"); n != 0 {
		t.Fatalf("swept before the group list loaded: %v", k.Fake.Calls())
	}
	k.Clock.Advance(30 * time.Second) // the next tick loads the list
	waitFor(t, "the sweep after the list loaded", func() bool { return sweeps(k) >= 1 })
	if n := k.Fake.Count("JoinLinkedGroup"); n != 0 {
		t.Fatalf("the sweep tried to join groups the bot is in: %v", k.Fake.Calls())
	}
}

// TestJoinSweepAfterTheListReads: the bot joining a group reads the group
// list again and sweeps; when that read fails the sweep waits for the read
// the next tick makes, so it never runs on a list that lacks the group.
func TestJoinSweepAfterTheListReads(t *testing.T) {
	k := modtest.New(t, "")
	// The re-read (the third read) waits until any sweep asked for before it
	// has ended, so a sweep on the list the failed read left shows. That
	// sweep would be waiting on the clock step that also brings the re-read.
	var reads atomic.Int32
	k.Fake.OnCall = func(call string) {
		if call != "JoinedGroups" || reads.Add(1) != 3 {
			return
		}
		time.Sleep(200 * time.Millisecond)
		for deadline := time.Now().Add(10 * time.Second); k.Logged("sweep") != sweeps(k) && time.Now().Before(deadline); {
			time.Sleep(2 * time.Millisecond)
		}
	}
	stop := startApp(t, k, newSweep(k), nil)
	defer stop()
	waitFor(t, "the sweep at connect", func() bool { return k.Logged("sweep") == 1 && sweeps(k) == 1 })
	groups := modtest.Groups()
	k.Fake.SetGroups(append(groups, client.Group{JID: G3, Name: "events", Parent: modtest.Community,
		Participants: groups[1].Participants})) // the bot is an admin there, as in general
	k.Fake.FailNext("JoinedGroups", errors.New("rate limited"))
	if err := k.Fake.Deliver(&client.JoinedGroup{Group: G3, Time: k.Clock.Now(),
		Info: client.Group{JID: G3, Parent: modtest.Community}}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the failed read after the join", func() bool { return k.Fake.Count("JoinedGroups") == 2 })
	k.Clock.Advance(30 * time.Second) // the next tick reads it again
	// Every sweep after the join checks the joined group's requests, unless it
	// ran on the list the failed read left. Counted once every sweep started
	// (one JoinRequests call in G1 each) has logged its end.
	g3 := func() int { return k.Fake.Count("JoinRequests " + string(G3)) }
	waitFor(t, "the sweep after the read", func() bool { return g3() >= 1 && k.Logged("sweep") == sweeps(k) })
	if after := sweeps(k) - 1; after != 1 || g3() != 1 {
		t.Fatalf("%d sweeps after the join, %d of them on the list with the joined group: one ran on the list "+
			"the failed read left", after, g3())
	}
}

// TestSweepRunsWhenWhatsAppNeverSaysCaughtUp: a connection where WhatsApp
// never says it delivered the offline backlog still gets its sweep, after
// a bounded wait.
func TestSweepRunsWhenWhatsAppNeverSaysCaughtUp(t *testing.T) {
	k := modtest.New(t, "")
	k.Fake.OnConnect = func(f *clienttest.Fake) { f.Emit(client.Lifecycle{Kind: client.Connected}) }
	stop := startApp(t, k, newSweep(k), nil)
	defer stop()
	waitFor(t, "the list at connect", func() bool { return k.Fake.Count("JoinedGroups") == 1 })
	waitFor(t, "the sweep waiting for the backlog", func() bool { return k.Logged(waitingForBacklog) == 1 })
	k.Clock.Advance(9 * time.Minute)
	time.Sleep(50 * time.Millisecond)
	if n := k.Fake.Count("SubGroups"); n != 0 {
		t.Fatalf("swept %d times before the wait for the offline backlog ran out", n)
	}
	k.Clock.Advance(time.Minute)
	waitFor(t, "the sweep after the wait", func() bool { return sweeps(k) >= 1 })
}

// TestSweepRunsWhenCaughtUpComesFirst: the library does not order its
// "caught up" and "connected" events; when WhatsApp says it delivered the
// offline backlog before the connection is reported, the connect sweep still
// runs at once, without waiting out the bounded wait for that word.
func TestSweepRunsWhenCaughtUpComesFirst(t *testing.T) {
	k := modtest.New(t, "")
	k.Fake.OnConnect = func(f *clienttest.Fake) {
		f.Emit(client.Lifecycle{Kind: client.CaughtUp, Detail: "0 offline events"})
		f.Emit(client.Lifecycle{Kind: client.Connected})
	}
	stop := startApp(t, k, newSweep(k), nil)
	defer stop()
	waitFor(t, "the sweep at connect", func() bool { return k.Logged("sweep") == 1 })
	if n := k.Logged("WhatsApp has not said it delivered the offline backlog; sweeping anyway"); n != 0 {
		t.Fatalf("the sweep waited for word WhatsApp had given (%d warnings)", n)
	}
}

// TestBotRemovedDetected: a leave event naming the bot raises a priority
// alert, the group shows absent at once, the next sweep does not alert again,
// and the bot never joins that group again by itself.
func TestBotRemovedDetected(t *testing.T) {
	k := modtest.New(t, "")
	s := newSweep(k)
	s.run(t)
	k.Deliver(&client.GroupChange{Group: modtest.G2, Actor: modtest.Admin, Time: k.Clock.Now(), Left: []client.JID{modtest.Bot}})
	reps := k.Reports(string(alert.BotRemoved))
	if len(reps) != 1 || !reps[0].Priority {
		t.Fatalf("bot_removed reports %+v, want one priority report", reps)
	}
	mustContain(t, reps[0].Text, "The bot was removed from jobs (group…0222) (community a)")
	if g := group(t, s.Coverage(), modtest.G2); g.State != store.CoverageAbsent {
		t.Fatalf("jobs after the removal: %+v", g)
	}
	// The event itself stores the group as absent, before any sweep runs.
	stored, err := k.Store.CoverageRows(k.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := stored[string(modtest.G2)]; r.State != store.CoverageAbsent || !r.JoinTried {
		t.Fatalf("stored jobs row after the event %+v", r)
	}
	s.run(t)
	if lost := texts(k, alert.CoverageLost); len(lost) != 0 {
		t.Fatalf("the sweep reported the removal again: %q", lost)
	}
	if n := k.Fake.Count("JoinLinkedGroup"); n != 0 {
		t.Fatalf("the bot rejoined a group it was removed from: %v", k.Fake.Calls())
	}
	rows, err := k.Store.CoverageRows(k.Ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r := rows[string(modtest.G2)]; r.State != store.CoverageAbsent || !r.JoinTried {
		t.Fatalf("stored jobs row %+v", r)
	}
	// Removed before the first sweep: still never joined by itself.
	k2 := modtest.New(t, "")
	k2.Fake.SetLinked(modtest.Community, []client.GroupRef{{JID: modtest.G1, Name: "general"}, {JID: modtest.G2, Name: "jobs"}})
	k2.Deliver(&client.GroupChange{Group: modtest.G2, Actor: modtest.Admin, Time: k2.Clock.Now(), Left: []client.JID{modtest.Bot}})
	s2 := newSweep(k2)
	s2.run(t)
	if n := k2.Fake.Count("JoinLinkedGroup"); n != 0 {
		t.Fatalf("joined a group the bot was removed from before the first sweep: %v", k2.Fake.Calls())
	}
	// Removed while no event reached the bot: WhatsApp's group list no longer
	// has "general". The sweep alerts once and does not join it by itself.
	all := modtest.Groups()
	groups := []client.Group{all[0], all[3]} // the community and the standalone group
	k2.Fake.SetGroups(groups)
	modtest.Load(t, k2.Dir, groups)
	s2.run(t)
	lost := k2.Reports(string(alert.CoverageLost))
	if len(lost) != 1 || !lost[0].Priority {
		t.Fatalf("coverage_lost reports %+v", lost)
	}
	mustContain(t, lost[0].Text, "The bot is no longer in general (group…0111) (community a): that group is not "+
		"moderated until a human admin adds the bot back and promotes it.")
	s2.run(t)
	if n := k2.Fake.Count("JoinLinkedGroup"); n != 0 || len(texts(k2, alert.CoverageLost)) != 1 {
		t.Fatalf("joins %d, alerts %q after another sweep", n, texts(k2, alert.CoverageLost))
	}
}

// TestDemotionEvent: a group-info event demoting the bot raises a priority
// alert, flips the group to not covered at once, and the sweep does not
// report it again.
func TestDemotionEvent(t *testing.T) {
	k := modtest.New(t, "")
	s := newSweep(k)
	s.run(t)
	k.Deliver(&client.GroupChange{Group: modtest.G2, Actor: modtest.Admin, Time: k.Clock.Now(), Demoted: []client.JID{modtest.BotPhone}})
	reps := k.Reports(string(alert.BotDemoted))
	if len(reps) != 1 || !reps[0].Priority {
		t.Fatalf("bot_demoted reports %+v, want one priority report", reps)
	}
	mustContain(t, reps[0].Text, "The bot is no longer an admin in jobs (group…0222) (community a)")
	if g := group(t, s.Coverage(), modtest.G2); g.State != store.CoverageNotAdmin {
		t.Fatalf("jobs after the demotion: %+v", g)
	}
	s.run(t)
	if lost := texts(k, alert.CoverageLost); len(lost) != 0 {
		t.Fatalf("the sweep reported the demotion again: %q", lost)
	}
	if n := k.Fake.Count("JoinRequests " + string(modtest.G2)); n != 1 {
		t.Fatalf("the sweep polled join requests where the bot is not an admin: %v", k.Fake.Calls())
	}
	// A demotion before the first sweep: the community's first list names the
	// group as not covered, and nothing reports it twice.
	k2 := modtest.New(t, "")
	k2.Deliver(&client.GroupChange{Group: modtest.G2, Actor: modtest.Admin, Time: k2.Clock.Now(), Demoted: []client.JID{modtest.Bot}})
	newSweep(k2).run(t)
	lists := texts(k2, alert.Coverage)
	if len(lists) != 2 || len(texts(k2, alert.CoverageLost)) != 0 || len(texts(k2, alert.BotDemoted)) != 1 {
		t.Fatalf("lists %q, coverage_lost %q", lists, texts(k2, alert.CoverageLost))
	}
	mustContain(t, lists[0], "The bot checked the 2 group(s) of community a:",
		"jobs (group…0222): the bot is in it but not an admin: a human admin must promote it")
}

// TestPermissionErrorMarksUncovered: WhatsApp refusing a removal, or a group
// query, as the bot is not an admin flips that group to not covered at once
// with one priority alert, however often it refuses.
func TestPermissionErrorMarksUncovered(t *testing.T) {
	k := modtest.New(t, "")
	refused := fmt.Errorf("%w: info query returned status 403", client.ErrNotAdmin)
	// A banned member is in "jobs"; `run` sweeps at connect and its executor
	// sends the removal, which WhatsApp refuses.
	groups := modtest.Groups()
	groups[2].Participants = append(groups[2].Participants, client.Participant{JID: modtest.Other1, LID: modtest.Other1})
	k.Fake.SetGroups(groups)
	k.Ban(modtest.Other1M)
	k.Fake.FailNext("Remove", refused)
	k.Enforcer.Wake = k.Exec.Wake
	s := newSweep(k)
	stop := startApp(t, k, s, k.Exec)
	waitFor(t, "the refusal reported", func() bool { return len(k.Reports(string(alert.CoverageLost))) == 1 })
	stop()
	lost := k.Reports(string(alert.CoverageLost))
	if len(lost) != 1 || !lost[0].Priority {
		t.Fatalf("coverage_lost reports %+v, want one priority report", lost)
	}
	mustContain(t, lost[0].Text, "The bot is not an admin in jobs (group…0222) (community a): WhatsApp refused to "+
		"remove a member there.")
	if g := group(t, s.Coverage(), modtest.G2); g.State != store.CoverageNotAdmin {
		t.Fatalf("jobs after the refusal: %+v", g)
	}
	if rows := k.Find(modtest.Other1M, store.ActRemove, modtest.G2); len(rows) != 1 || rows[0].Status != store.Failed {
		t.Fatalf("removal rows %+v", rows)
	}
	// The same refusal again does not alert again.
	s.LostAdmin(k.Ctx, modtest.G2, "WhatsApp refused to remove a member")
	if n := len(texts(k, alert.CoverageLost)); n != 1 {
		t.Fatalf("%d alerts after a second refusal, want 1", n)
	}
	// A group query refused: the sweep's join-request poll in "general".
	k.Fake.FailNext("JoinRequests", refused)
	s.run(t)
	lost = k.Reports(string(alert.CoverageLost))
	if len(lost) != 2 {
		t.Fatalf("coverage_lost reports %+v, want one more for general", lost)
	}
	mustContain(t, lost[1].Text, "The bot is not an admin in general (group…0111) (community a): WhatsApp refused to "+
		"list its join requests there.")
	if g := group(t, s.Coverage(), modtest.G1); g.State != store.CoverageNotAdmin {
		t.Fatalf("general after the refusal: %+v", g)
	}
	s.run(t)
	if n := len(texts(k, alert.CoverageLost)); n != 2 {
		t.Fatalf("%d alerts after another sweep, want 2", n)
	}
}

// TestPeriodicAdminRecheck: every reconcile interval the app relearns the
// groups from WhatsApp and sweeps, so a demotion no event reported (the bot
// was offline) is found and alerted once per episode.
func TestPeriodicAdminRecheck(t *testing.T) {
	k := modtest.New(t, "")
	s := newSweep(k)
	stop := startApp(t, k, s, nil)
	defer stop()
	waitFor(t, "the sweep at connect", func() bool { return sweeps(k) == 1 })
	demoted := modtest.Groups()
	demoted[2].Participants[0].IsAdmin = false // "jobs"
	interval := func(groups []client.Group, n int) {
		t.Helper()
		k.Fake.SetGroups(groups)
		k.Clock.Advance(time.Hour)
		waitFor(t, fmt.Sprintf("sweep %d", n), func() bool { return sweeps(k) == n })
	}
	interval(demoted, 2)
	lost := k.Reports(string(alert.CoverageLost))
	if len(lost) != 1 || !lost[0].Priority {
		t.Fatalf("coverage_lost reports %+v, want one priority report", lost)
	}
	mustContain(t, lost[0].Text, "The bot is no longer an admin in jobs (group…0222) (community a)")
	interval(demoted, 3)
	if n := len(texts(k, alert.CoverageLost)); n != 1 {
		t.Fatalf("%d alerts after another interval, want 1", n)
	}
	// Promoted again (covered, logged only), then demoted again: a new episode.
	interval(modtest.Groups(), 4)
	if g := group(t, s.Coverage(), modtest.G2); g.State != store.CoverageCovered {
		t.Fatalf("jobs after the promotion: %+v", g)
	}
	interval(demoted, 5)
	if n := len(texts(k, alert.CoverageLost)); n != 2 {
		t.Fatalf("%d alerts after a second demotion, want 2", n)
	}
	if n := k.Fake.Count("JoinedGroups"); n < 5 {
		t.Fatalf("the group list was relearned %d times, want once per sweep", n)
	}
}

// TestAutoJoinAbsentLinkedGroups: the bot asks to join, by itself, every
// linked group of a community it is not in (the announcement group too), at
// the first sweep and when a new linked group appears; WhatsApp joins it or
// holds it for approval, and either is reported. It asks once per group.
func TestAutoJoinAbsentLinkedGroups(t *testing.T) {
	k := modtest.New(t, "")
	k.Fake.SetLinked(modtest.Community, []client.GroupRef{{JID: Ann, Name: "announcements", IsAnnouncement: true},
		{JID: modtest.G1, Name: "general"}, {JID: modtest.G2, Name: "jobs"}})
	k.Fake.JoinPending = map[client.JID]bool{Ann: true}
	s := newSweep(k)
	res := s.run(t)
	joins := func() []string {
		var out []string
		for _, c := range k.Fake.Calls() {
			if strings.HasPrefix(c, "JoinLinkedGroup") {
				out = append(out, c)
			}
		}
		return out
	}
	if j := joins(); len(j) != 1 || j[0] != "JoinLinkedGroup "+string(modtest.Community)+" "+string(Ann) || res.Joins != 1 {
		t.Fatalf("joins %v (result %+v), want only the announcement group", j, res)
	}
	lists := texts(k, alert.Coverage)
	if len(lists) != 2 {
		t.Fatalf("lists %q", lists)
	}
	mustContain(t, lists[0], "announcements (group…0777): the bot asked to join: an admin of that group must approve "+
		"it, then promote the bot")
	// A linked group added later is joined when it appears.
	k.Fake.SetLinked(modtest.Community, []client.GroupRef{{JID: Ann, Name: "announcements", IsAnnouncement: true},
		{JID: modtest.G1, Name: "general"}, {JID: modtest.G2, Name: "jobs"}, {JID: G3, Name: "events"}})
	s.run(t)
	if j := joins(); len(j) != 2 || j[1] != "JoinLinkedGroup "+string(modtest.Community)+" "+string(G3) {
		t.Fatalf("joins %v, want events joined", j)
	}
	reps := texts(k, alert.Coverage)
	if len(reps) != 3 {
		t.Fatalf("reports %q", reps)
	}
	mustContain(t, reps[2], "New group in community a: events (group…0333): the bot joined it by itself: a human admin "+
		"must promote it.")
	// Neither is asked again while the bot is not yet seen in it.
	s.run(t)
	if j := joins(); len(j) != 2 {
		t.Fatalf("joins %v, want no new attempt", j)
	}
	// Standalone sets have no community to join through.
	for _, c := range joins() {
		if strings.Contains(c, string(modtest.GB)) {
			t.Fatalf("joined a standalone group: %v", c)
		}
	}
}

// TestAutoJoinFailureFallsBackToJoinRequest: when WhatsApp refuses the join
// (after waiting out its rate limit) the admins are asked for a /join link
// instead, and the bot does not try that group by itself again. A
// standalone group the bot is not in is always a /join request.
func TestAutoJoinFailureFallsBackToJoinRequest(t *testing.T) {
	k := modtest.NewConfig(t, strings.Replace(modtest.Config, `groups: ["99999000000888@g.us"]`,
		`groups: ["99999000000888@g.us", "99999000000777@g.us"]`, 1))
	k.Fake.SetLinked(modtest.Community, []client.GroupRef{{JID: modtest.G1, Name: "general"},
		{JID: modtest.G2, Name: "jobs"}, {JID: G3, Name: "events"}})
	limited := fmt.Errorf("%w: info query returned status 429", client.ErrRateLimited)
	k.Fake.FailNext("JoinLinkedGroup", limited, errors.New("info query returned status 501: feature-not-implemented"))
	s := newSweep(k)
	s.run(t)
	if n := k.Fake.Count("JoinLinkedGroup " + string(modtest.Community) + " " + string(G3)); n != 2 {
		t.Fatalf("asked %d times, want 2 (a rate limit, then a refusal)", n)
	}
	if slept := k.Clock.Slept(); len(slept) != 1 || slept[0] != 30*time.Second {
		t.Fatalf("waits %v, want one 30s back-off", slept)
	}
	lists := texts(k, alert.Coverage)
	if len(lists) != 2 {
		t.Fatalf("lists %q", lists)
	}
	mustContain(t, lists[0], "events (group…0333): the bot could not join it by itself (info query returned status "+
		"501: feature-not-implemented): send /join <invite link> for it")
	mustContain(t, lists[1], "The bot checked the 2 group(s) of set b:",
		"group…0777: the bot is not in it: send /join <invite link> for it")
	s.run(t)
	if n := k.Fake.Count("JoinLinkedGroup"); n != 2 {
		t.Fatalf("tried again: %v", k.Fake.Calls())
	}
	if g := group(t, s.Coverage(), G3); g.State != store.CoverageAbsent {
		t.Fatalf("events: %+v", g)
	}
}
