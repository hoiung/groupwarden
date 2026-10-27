# Config reference

`schema/config.schema.json` is the only place a key, its default and its limits are written down; this page says which code reads each key. `examples/config.yaml` is a working starting point.

How a config is read (`internal/config`):

1. YAML 1.2 is parsed by hand over `go.yaml.in/yaml/v3` nodes, so `NO`, `0800` and `1.10` are never turned into a boolean or a number. A duplicate key, an anchor, an alias, a tag, a `<<` merge, a second document, and any key named `regex` or `pattern` are refused (`yamltree.go`).
2. The schema's defaults are filled in, then the tree is validated against the schema (`schema.go`). Errors read `line L: <what is wrong> (column C)`.
3. Words and phrases must be quoted, and the rules are compiled (`config.go` `Parse` → `rules.Compile`), which checks that every rule that deletes, removes and bans needs a keyword plus a link or contact detail, or keywords from two word lists, on every way it can match. When a message is decided, those two lists must match different words of it, so a word that two lists share (or that a wildcard in each matches) is still one keyword, however each list reads it: capitals, look-alike letters, digit swaps or spaced-out letters all map back to the same characters of the message (`decide.go` `acts`, `normalise.go` `Words`). A `*` reaches only as far as a break between words that any reading shows, so `profit|bitcoin` is two words for `"daily profit*"` and `"*coin"` even though the look-alike reading turns `|` into `l` (`normalise.go` `stands`).
4. When `corpus_dir` is set, the compiled rules must catch every spam sample and no legit sample (`reload.go` `checkCorpus`). Legit samples are also tested under each community's own words and disabled rules; spam is judged under the global rules, since a community may disable a rule (`corpus.go` `legitHits`).

A reload (SIGHUP) runs the same steps on the new file and swaps the whole result in at once; if any step fails, the bot keeps the last good config, which is also kept on disk as `data_dir/config.last-good.yaml` (`reload.go` `Holder`). While the file is refused, `run` at start, `healthcheck`, the backups and the commands that read the bot's data (`restore`, `schedule`, `ledger`, `ban`, `member`, `pair`, `groups`, `resolve-link`) use that copy too and print a note saying so; `check` and `corpus test` report the file itself, `sync-config` checks the new commit, and `corpus add` reads no config (`reload.go` `LoadRunning`); with no last good copy they refuse to start.

On a node the config comes from the deployer's private config repo: `groupwarden sync-config`, run by `deploy/groupwarden-sync.timer`, pulls it, runs the same steps on the new commit, swaps the live config directory to a copy of that commit (config and corpus together) and sends the running bot SIGHUP (`internal/configsync`). A failed pull or a rejected commit alerts the admin chat once and shows in `/status`; the bot keeps its config.

## Keys and the code that reads them

| Key | Read by |
|---|---|
| `data_dir` | `cmd/groupwarden/whatsapp.go` `runBot` (single-instance lock, which names the holding process and command); `cmd/groupwarden/main.go` (WhatsApp session store); `internal/config/config.go` `StoreDB`, `WhatsmeowDB`; `internal/config/reload.go` (last good copy); `cmd/groupwarden/storecmds.go` `check --secrets`; `internal/configsync` `Run` (finds the running bot through the lock; must lie outside the config repo); `cmd/groupwarden/synccmd.go` `syncConfig` (records the sync result); `cmd/groupwarden/backupcmd.go` `backup` (the backup's scratch copy), `restore` (takes the lock, writes `groupwarden.db`) |
| `secrets_file` | `cmd/groupwarden/storecmds.go` `check --secrets` (`config.ReadSecretsFile`); `cmd/groupwarden/whatsapp.go` `adminChatOptions` (`run` refuses to start without the admin chat's bot token and chat ID); `internal/configsync` `checkRelease` (must lie outside the config repo) |
| `deploy_key_file` | `cmd/groupwarden/storecmds.go` `check --secrets`; `internal/configsync` `sshEnv` (the sync's `git pull`), `checkRelease` (must lie outside the config repo) |
| `corpus_dir` | `internal/config/reload.go` `checkCorpus`. A relative path is read from the config file's directory, so `corpus_dir: corpus` in the private config repo moves with each synced copy |
| `act_on_replay_max_age` | `internal/pipeline/worker.go` (replayed messages older than this are only reported); `internal/action/executor.go` `recheck` (a queued delete of an older message is dropped, and a priority report tells the admins to delete it by hand); `internal/pipeline/admin.go` `Ban` ([Ban] on a watch-only report deletes the message only while it is younger); an edit is aged by its original's server time in all three (`internal/pipeline/worker.go` `targetSent`); `internal/pipeline/admin.go` `postDeletes` (every `/ban` deletes the member's other posts on record only while they are younger: an older one is recorded failed, counted in the reply and never sent) |
| `deafness_alert_hours` | `internal/app/app.go` `SettingsFrom` → `monitor.go` |
| `disconnect_alert_minutes` | `internal/app/app.go` `SettingsFrom` → `monitor.go` |
| `config_sync_minutes` | `internal/app/app.go` `outsideTimers` / `watchedTimers` → `checkOverdue` (a priority alert when the sync has not run for twice this); `cmd/groupwarden/synccmd.go` `schedule` (`groupwarden schedule sync` prints the sync timer's `OnCalendar` value for the install script), `syncConfig` (the failure alert says when it tries again) |
| `heartbeat_url` | `internal/app/heartbeat.go` `pinger`: read at every ping, so a reload turns it on or off; pinged every 5 minutes only while the `healthcheck` rule passes (`health.go` `ReadHealth`), so an outside monitor notices a node that is off or asleep. Errors never quote the URL |
| `daily_check_time` | `internal/app/dailycheck.go` `checkDaily`: read at every monitor tick, so a reload moves it; once a local day at or after this time (HH:MM, the node's local time zone: `TZ` or `/etc/localtime`) the admin chat gets "Daily check, Tuesday 6 October 2026: groupwarden is alive and working.", or what is not working (`health.go` `Problems`, the `healthcheck` rule). The day posted is kept in `groupwarden.db` (`daily_check_day`), so a restart does not post twice |
| `mode` | `internal/rules/compile.go` (the global scope's mode) |
| `retention.evidence_days`, `retention.action_log_months`, `retention.announcement_secret_days` | `internal/config/config.go` `checkRetention` (`announcement_secret_days` at least `evidence_days`; `action_log_months`, a month counted as 28 days, at least `evidence_days` plus `report.attachment_show_hours` and a day, so no report is purged while its text is still to be stripped or its attachment taken down), `RetentionFor`, `LongestRetention`; `internal/ledger/purge.go` `Purge` (evidence copies and files, action log, reports, message secrets once the group list has been read, so announcement groups are known); `internal/telegram/deliver.go` `stripDue` (the admin chat's reports lose the message text after `evidence_days`) |
| `rate.per_minute`, `rate.burst` | `internal/action/executor.go` `takeToken` (the outbox token bucket) |
| `breaker.max_actions`, `breaker.window_minutes` | `internal/action/executor.go` `breaker` |
| `reconcile.interval_minutes` | `internal/app/app.go` `SettingsFrom` (linked-device check, the group-list refresh and the sweep: coverage and its reports, joining absent linked groups, banned members present, join requests, phone-only bans). Every ban from the admin chat (`/ban <number>`, `/ban` as a reply, [Ban], [Add to ban list]) asks for a sweep at once instead of waiting for the interval (`internal/app/controls.go` `BanNumber`, `sweepAfter`) |
| `backup.target_dir` | `cmd/groupwarden/storecmds.go` `check --secrets` (must be on a different disk); `cmd/groupwarden/backupcmd.go` `backup` (the same check every night, then `internal/backup` `Run`); `groupwarden backup-dir` (the install script lets the backup unit write there) |
| `backup.age_recipient` | `cmd/groupwarden/storecmds.go` `check --secrets` (parsed by `age`); `internal/backup` `Run` (each backup is encrypted to it) |
| `backup.keep` | `internal/backup` `Run` → `prune` (keeps the newest this many backups; other files in the directory are never touched) |
| `evidence.max_attachment_mb` | `internal/pipeline/moderator.go` `evidence` (an attachment that declares a size over it is recorded by type, name and size only); `internal/action/media.go` `fetchOne` (the download stops at it, whatever size the post declared, and a file over it is recorded the same way) |
| `report.attachment_show_hours` | `internal/telegram/deliver.go` `takeDownDue` (an attachment posted to the admin chat is deleted after it), `takeDown` (the placeholder's text), `postAttachment` and `uploadFailed` (an attachment Telegram keeps refusing is given up after it, marked failed, and a reply under its report says so); `internal/telegram/updates.go` `show` (the reply says when the repost comes down, or why the attachment was not posted) |
| `bans.scope` | `internal/rules/compile.go`, `decide.go` (`Decision.BanIn`); `internal/pipeline/admin.go` `BanNumber` (`/ban <number>` bans everywhere under `all_communities`; under `per_community` it is refused, as `groupwarden ban add` without `--community` is) and `postDeletes` (a ban deletes posts in the groups its scope covers) |
| `word_lists` | `internal/rules/compile.go` `addWords` |
| `leet_word_lists` | `internal/rules/compile.go` |
| `never_match` | `internal/rules/compile.go`; `internal/normalise` `Prepare` |
| `allowed_domains` | `internal/rules/compile.go`; `conditions.go` `linkSignals` |
| `rules.min_word_length` | `internal/rules/compile.go` `addWords` |
| `rules.list` (`name`, `action`, `confirmed`, `on`, `when`) | `internal/rules/compile.go` `compileRules`, `decide.go` |
| `communities.<id>` (`name`, `groups`, `mode`, `disable_rules`, `word_lists`) | `internal/rules/compile.go` `compileCommunities`; `internal/pipeline/directory.go`. The admin chat's `/enable` adds to these without editing the file: the bot keeps that list in `groupwarden.db` and every load merges it in (`internal/config/enabled.go`), a community with the global settings, a single group as a set of one under its ID, named with the group's WhatsApp name (the name `/status` shows); an entry the file already lists keeps the file's settings, and `/disable` cannot switch off what the file lists. The merged list leaves the config version as the file's (`internal/config/config.go` `parseEnabled`), so `check` prints the version the bot posts. A group ID in `groups` is a number ending in `@g.us`, or an older group's `<number>-<number>@g.us` (`compile.go` `groupID`) |
| `communities.<id>.retention` | `internal/config/config.go` `RetentionFor` |
| `acl.super_admins` | `internal/telegram/roles.go` `decide`, the one role check for every command and button (a super admin may do everything); `internal/telegram/menu.go` `commandList` reads only whether the section is there (with it, the pinned list and the reply to any other command say who may use what); `internal/config/config.go` `ACL.check` (one role per person). See [Admin-chat access](#admin-chat-access-acl) |
| `acl.admins[].user` | `internal/telegram/roles.go` `decide` (a community admin: `/status`, `/enable` and `/disable` for their own, and what a member may do); `internal/config/config.go` `ACL.check` (listed once) |
| `acl.admins[].assigned` | `internal/telegram/roles.go` `decide` returns it and the gate in `internal/telegram/updates.go` puts it on the `Actor`; `internal/app/enabled.go` `mayTarget` (`Enable`, `Disable`: a community admin's target must be one of these, or a group inside one, before anything is recorded or joined) |

## Writing words and rules

- Every word and phrase is in quotes. Matching is whole-word and ignores case, accents, look-alike letters (Unicode UTS #39), invisible characters and letters spaced out with single spaces or dots.
- A `*` may stand at the start or end of a word (`"airdrop*"`, `"*coin"`), never in the middle or on its own.
- Punctuation inside an entry splits it the same way it splits a message: `"1.10"` is the phrase `1 10`.
- A single word shorter than `rules.min_word_length` is refused; a phrase is not.
- A rule's `when:` is one of `all:`, `any:`, `words:` (one list name or several, any of them) or `has:` (built-in signals: `any_link`, `invite_link`, `shortener`, `phone_number`, `contact_card`, `handle`, `money_amount`).
- `any_link` counts a link written with a scheme (`https://`), `www.`, a path, a port or a query, an invite host (`t.me`, `wa.me`) or a name under one (`x.t.me`), and a bare name (`cryptoprofits.com`) only when it ends in `co`, `com`, `net` or `org` and a country code (`promo.co.in`) or in one of the endings spam uses that are not English words (`com`, `org`, `io`, `ly`, `xyz`, `icu`, `cyou`: `internal/rules/conditions.go` `bareEndings`). Most of the 1,400 endings are also words, file endings or stock exchange codes, so a missing space after a full stop ("down again.Is it time", "at 5pm.So", "went up.To the moon", "up.Net profit"), a file name (`notes.zip`), a share code (`SAP.DE`) and code (`std::cout`) are text, and so is a bare `earnbig.me` or `earnbig.top`, which reads the same as "again.Me": a wrong ban is the worse mistake, and a post with only that is turned into a rule with the spam-intake skill. A shortener counts with its path (`bit.ly/x`); its host alone is read like any other name. In a document's file name no bare name counts. `phone_number` counts a run of digits holding a number made of whole groups from its first group, or from a later group of two or more digits after a space ("Room 4 07700 …", "Room 4 0044 …"; never "000", nor after "-" or ".", as in "2025-11-07" or "978-0306406157"), unless the run is written like a large number ("1 000 000 000", "345 012 789 456"), which is read from its first group only. A number written only in pairs ("06 12 34 56 78") runs to the end of the digits or a date, so a list of more pairs than a number has ("04 11 24 35 42 08 15 22 31") is not one. It does not count digits that are only dates and times: with the dates ("07.11.2025", "2025-11-07" with a 19xx or 20xx year, unless the digits go on past a two-digit year in pairs, "06.12.34.56.78") set aside, what is left has no `+`, no group of three or more digits, and fewer than five digits or groups not all joined by the same separator ("01.12.2025 - 05.12.2025 18:00", "08.30 07.11.2025", "09.30 - 10.30 - 11.30"); a number written with its last digits like a date keeps its first ones and still counts. Nor does a decimal that starts "0." ("0.00012345 BTC"). `corpus add` redacts the whole run, so a number followed by more digits is not left behind. Each field is searched in windows of 1,024 bytes (a link the window's end may have cut short is searched again from its start); a field with more than 64 link-shaped matches (links, email addresses, and text such as "again.Is" or "1.1.1.1", each of which costs a search) counts as having a link, an invite or a shortener written with its path anywhere in it still counts as one, and its text after them is still read for phone numbers and handles.
- A rule with `action: delete_remove_ban` acts only when its community is in `enforce` mode and the rule has `confirmed: true`; until then it reports what it would have done.
- A rule with `action: log` never acts and posts no report of its own: its matches are counted in the daily summary (one line per rule and community). The copy of such a match keeps its text and describes its attachment (type, name, size) without downloading it.
- `on: push_name` matches the sender's display name on its own; it is never combined with the message.

## Admin-chat access (acl)

Without an `acl:` section, every Telegram admin of the admin chat can use every command and button, and nobody else can use any. With one, the file alone decides who may do what, by Telegram user ID: being a Telegram admin of the chat gives no extra power in the bot, and anyone else in the chat is a member.

```yaml
acl:
  super_admins: [111111111]
  admins:
    - user: 222222222
      assigned: ["99999000000999@g.us"]
```

- **super admin** (`super_admins`, at least one): every command and button.
- **community admin** (`admins`): `/status`, and `/enable` and `/disable` for the communities and groups in their `assigned` list (a group inside an assigned community counts as theirs), plus everything a member can do.
- **member** (anyone else in the chat): `/ban`, `/ban <number>`, `/unban`, `/unban <number>` and the buttons that act on one report.

| Action | Member | Community admin | Super admin |
|---|---|---|---|
| `/ban`, `/ban <number>`, `/unban`, `/unban <number>`, [Ban], [Undo], [Add to ban list] | yes | yes | yes |
| [No], [Show attachment] (they change nothing) | yes | yes | yes |
| `/status` | no | yes | yes |
| `/enable`, `/disable` | no | only an `assigned` entry, or a group inside an assigned community | yes, anything |
| `/pause`, `/resume`, `/reload`, [Resume] | no | no | yes |
| [Done] (the bot-phone reminder) | no | no | yes |
| any other command (the reply is the command list) | yes | yes | yes |

A refused command or button gets a reply naming who may use it and changes nothing. An edit to the section takes effect at the next config sync or `/reload`, with no restart. `groupwarden check` refuses a section with no super admin, a user ID that is not a positive whole number, an `assigned` entry that is not a community or group ID, or one person listed twice (as two community admins, or as a super admin and a community admin); the last good config keeps running.

Finding a Telegram user ID: Telegram's apps do not show it. The bot's journal shows it as `user` on the line it writes when it refuses a command: without `acl:`, have the person (not a Telegram admin of the chat) send `/status` (`command from a non-admin ignored`); with `acl:`, a member's `/status` (`command refused`). The Bot API's `getChatAdministrators` for the admin chat lists every Telegram admin's `user.id`.

Finding a community or group ID for `assigned`:

- a WhatsApp community the config file lists: its key under `communities:`;
- a group in a set the file lists: its ID under that set's `groups:` (a set's own key is a name, not an `assigned` entry);
- one switched on with `/enable`: `groupwarden ledger summary` once the bot has acted there, or `groupwarden groups` with the service stopped (it takes the data directory's lock);
- a new community: the super admin sends `/enable` with its invite link first, then its ID goes into `assigned`.

## Banning from the admin chat

- `/ban <number>` bans a phone number, for a spammer the bot posted no report about. Write it with its country code; `+`, spaces, dashes and brackets are fine (`+44 7700 900123`, `447700900123`, `+44 (7700) 900-123`), and a `(0)` written after the country code is dropped. Once those are taken out it must be 8 to 15 digits and not start with 0, or the reply is the usage text (`groupwarden ban` and `groupwarden member` read a phone number the same way: `internal/client/phone.go`). It is stored as `groupwarden ban add <phone>` stores it: keyed by the phone number until its LID is found, everywhere under `bans.scope: all_communities` (under `per_community` it is refused: use `groupwarden ban add --community`, or `/ban` as a reply to a report from that community). Its ledger rows name who sent it (trigger `tg:ban:<time>`). Its posts are found by the number and by its LID, looked for in this order: the group list's pair, the saved WhatsApp IDs, WhatsApp (one lookup), then the bot's own records (the record of a post, or a ban still in force), so a post sent without the number is found too. That LID also resolves the ban at once, as the sweep would resolve it, so a post they send under their LID alone right after the ban is deleted; with no LID known, the sweep looks it up. Until the LID is found (`groupwarden ban add <phone>` finds it at once in the saved WhatsApp IDs or groupwarden's records; otherwise a `/ban <number>` that names it, or the sweep, finds it), a later post, join or join request of theirs that names their LID alone, the sweep's check of the members already in a group, and a delete the bot had already queued for one of their posts are matched to the ban through the group list's pair for them or the record of an earlier post that carried both. A number already banned everywhere gets a reply saying so and no second ban (the ban records this admin's when it was the bot's), but its posts are deleted, its ban resolved on the LID known and a sweep asked for all the same: every `/ban` cleans up. A current admin's number is refused, as [Ban] refuses one: an admin must decide. The bot's own number is never banned.
- `/ban` sent alone as a reply to a "would have acted" report acts on that report, as [Ban] does.
- Every ban from the admin chat that puts someone on the ban list (`/ban <number>`, also for a number already banned, and `/ban` as a reply, [Ban] and [Add to ban list] in a community in enforce mode) asks for a sweep at once, so a banned member the bot finds in a group is removed without waiting for `reconcile.interval_minutes` (in a community in shadow mode the sweep only reports them, as it does every banned member there). In a community in shadow mode a report-form ban is only recorded, so there is no sweep.
- Until the bot has read its group list (the first seconds after a start, longer while it cannot read it from WhatsApp) every ban is refused with a reply to try again in a minute: a ban planned then would find no group to delete the person's posts in.
- Every ban from the admin chat also deletes the person's posts the bot still has a record of: each post it decided in the last 72 hours, in every group the ban covers, found by either address WhatsApp gave for its author and deleted under the one the group knew them by. The deletes go through the outbox like any other (`rate`, the check just before each is sent, a `/pause`); like every delete they carry on while the `breaker` holds removals and bans. Each post is deleted on its own, several in one group included, and two bans of one person delete a post only once, whichever address each names them by. A post older than `act_on_replay_max_age` cannot be deleted by an admin any more: it is recorded as failed, counted once in the reply and never sent. A delete WhatsApp refused (it failed all of up to 8 tries, or WhatsApp said the bot is not an admin there) is reported once and never tried again, even by a later ban; one the bot dropped before sending it (an `/unban` got there first, the person was an admin then, the rules no longer match the post) is planned again by the next ban. Posts in a community in shadow mode, or where the person is a current admin, are left as they are. The reply says how many posts are being deleted and how many could not be. Posts decided before this version (store migration 12) have no sender on record and are not found.
- A post by a banned member is deleted when it arrives, in a community in enforce mode: one sent after the ban and before the sweep removes them, or after they rejoined and before their removal. A ban a pause holds does not count until it is applied (the pause may be the breaker's, held because a rule may be misfiring). A post a log rule or a watch-only rule matches is reported as before and deleted as well; one a confirmed rule matches is deleted by that rule. The delete itself is not reported (the admins were told of the ban or the rejoin); the log line `post by a banned member; delete planned` and its ledger row (trigger `banned:<message ID>`) record it. A banned member who is a current admin is left alone, as for every action.
- `/unban <number>` lifts every ban of a phone number, whether it is still keyed by the number or already resolved to a LID, and any ban a pause still holds for them; its ledger row names who sent it (trigger `tg:unban:<time>`). It finds a ban by the number and by its LID, looked for as `/ban <number>` looks for it, so a ban that names them by LID alone is lifted too. Their deletes and removals still queued are cancelled; one whose call had already gone out is reported, naming the group to re-invite them to. A number of 8 or more digits is a phone number; `#N` or a shorter number is a report number (`/unban #12` undoes report 12, as [Undo] does). Unbanning adds nobody back to a group: re-invite them by hand.
- [Undo] (and `/unban #N`), an admin re-adding someone by hand and
  `groupwarden ban remove <lid>` lift the bans of the person they name. WhatsApp
  gives a number to someone new once its holder leaves it, so a number the bot
  finds for a person in its records (a post, a ban) counts as theirs unless
  someone else holds it now: the group list pairs it with someone else or, when
  the group list has no pair for it, the latest the bot recorded of the number
  (a post, a ban, an entry on the ban list, whichever came last) names someone
  else (the command line reads the saved WhatsApp IDs, then that). The same goes
  for the number a ban puts on the list when it names the person by one address
  ([Add to ban list], `groupwarden ban add <lid>`, and [Ban] when the reported
  post carried one address; with both, [Ban] takes the two). A ban of another
  WhatsApp ID that holds the person's number (someone who had it before) is not
  lifted: it stays on
  that ID and only the number is taken off it, so it no longer covers the person
  named; [Undo]'s reply and `groupwarden ban remove` count it apart from the
  bans lifted. A lift of someone who had a number before lifts their own ban
  alone: the ban of whoever holds the number now keeps it. An admin re-adding a
  number alone (in a group that lists its members by number) re-adds whoever the
  bot knows by that number now. `/unban <number>` and
  `groupwarden ban remove <phone>` name a number and lift every ban of it,
  whoever's. The bot goes no further than that: when two people who held one
  number at different times were both banned, the latest it recorded of the
  number decides whose it is, and that can be out of date. After lifting either
  of them, `groupwarden ban list` shows which ban still carries the number.
