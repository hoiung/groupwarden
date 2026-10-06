# Config reference

`schema/config.schema.json` is the only place a key, its default and its limits are written down; this page says which code reads each key. `examples/config.yaml` is a working starting point.

How a config is read (`internal/config`):

1. YAML 1.2 is parsed by hand over `go.yaml.in/yaml/v3` nodes, so `NO`, `0800` and `1.10` are never turned into a boolean or a number. A duplicate key, an anchor, an alias, a tag, a `<<` merge, a second document, and any key named `regex` or `pattern` are refused (`yamltree.go`).
2. The schema's defaults are filled in, then the tree is validated against the schema (`schema.go`). Errors read `line L: <what is wrong> (column C)`.
3. Words and phrases must be quoted, and the rules are compiled (`config.go` `Parse` → `rules.Compile`), which checks that every rule that deletes, removes and bans needs a keyword plus a link or contact detail, or keywords from two word lists, on every way it can match.
4. When `corpus_dir` is set, the compiled rules must catch every spam sample and no legit sample (`reload.go` `checkCorpus`).

A reload (SIGHUP) runs the same steps on the new file and swaps the whole result in at once; if any step fails, the bot keeps the last good config, which is also kept on disk as `data_dir/config.last-good.yaml` (`reload.go` `Holder`).

On a node the config comes from the deployer's private config repo: `groupwarden sync-config`, run by `deploy/groupwarden-sync.timer`, pulls it, runs the same steps on the new commit, swaps the live config directory to a copy of that commit (config and corpus together) and sends the running bot SIGHUP (`internal/configsync`). A failed pull or a rejected commit alerts the admin chat once and shows in `/status`; the bot keeps its config.

## Keys and the code that reads them

| Key | Read by |
|---|---|
| `data_dir` | `cmd/groupwarden/whatsapp.go` `runBot` (single-instance lock, which names the holding process and command); `cmd/groupwarden/main.go` (WhatsApp session store); `internal/config/config.go` `StoreDB`, `WhatsmeowDB`; `internal/config/reload.go` (last good copy); `cmd/groupwarden/storecmds.go` `check --secrets`; `internal/configsync` `Run` (finds the running bot through the lock; must lie outside the config repo); `cmd/groupwarden/synccmd.go` `syncConfig` (records the sync result); `cmd/groupwarden/backupcmd.go` `backup` (the backup's scratch copy), `restore` (takes the lock, writes `groupwarden.db`) |
| `secrets_file` | `cmd/groupwarden/storecmds.go` `check --secrets` (`config.ReadSecretsFile`); `cmd/groupwarden/whatsapp.go` `adminChatOptions` (`run` refuses to start without the admin chat's bot token and chat ID); `internal/configsync` `checkRelease` (must lie outside the config repo) |
| `deploy_key_file` | `cmd/groupwarden/storecmds.go` `check --secrets`; `internal/configsync` `sshEnv` (the sync's `git pull`), `checkRelease` (must lie outside the config repo) |
| `corpus_dir` | `internal/config/reload.go` `checkCorpus`. A relative path is read from the config file's directory, so `corpus_dir: corpus` in the private config repo moves with each synced copy |
| `act_on_replay_max_age` | `internal/pipeline/worker.go` (replayed messages older than this are only reported); `internal/action/executor.go` `recheck` (a queued delete of an older message is dropped); `internal/pipeline/admin.go` `Ban` ([Ban] on a watch-only report deletes the message only while it is younger) |
| `deafness_alert_hours` | `internal/app/app.go` `SettingsFrom` → `monitor.go` |
| `disconnect_alert_minutes` | `internal/app/app.go` `SettingsFrom` → `monitor.go` |
| `config_sync_minutes` | `internal/app/app.go` `timers` → `checkOverdue` (a priority alert when the sync has not run for twice this); `cmd/groupwarden/synccmd.go` `schedule` (`groupwarden schedule sync` prints the sync timer's `OnCalendar` value for the install script), `syncConfig` (the failure alert says when it tries again) |
| `heartbeat_url` | `internal/app/heartbeat.go` `pinger`: read at every ping, so a reload turns it on or off; pinged every 5 minutes only while the `healthcheck` rule passes (`health.go` `ReadHealth`), so an outside monitor notices a node that is off or asleep. Errors never quote the URL |
| `daily_check_time` | `internal/app/dailycheck.go` `checkDaily`: read at every monitor tick, so a reload moves it; once a local day at or after this time (HH:MM, the node's local time zone: `TZ` or `/etc/localtime`) the admin chat gets "Daily check, <date>: groupwarden is alive and working.", or what is not working (`health.go` `Problems`, the `healthcheck` rule). The day posted is kept in `groupwarden.db` (`daily_check_day`), so a restart does not post twice |
| `mode` | `internal/rules/compile.go` (the global scope's mode) |
| `retention.evidence_days`, `retention.action_log_months`, `retention.announcement_secret_days` | `internal/config/config.go` `checkRetention`, `RetentionFor`, `LongestRetention`; `internal/ledger/purge.go` `Purge` (evidence copies and files, action log, reports, message secrets); `internal/telegram/deliver.go` `stripDue` (the admin chat's reports lose the message text after `evidence_days`) |
| `rate.per_minute`, `rate.burst` | `internal/action/executor.go` `takeToken` (the outbox token bucket) |
| `breaker.max_actions`, `breaker.window_minutes` | `internal/action/executor.go` `breaker` |
| `reconcile.interval_minutes` | `internal/app/app.go` `SettingsFrom` (linked-device check, the group-list refresh and the sweep: coverage and its reports, joining absent linked groups, banned members present, join requests, phone-only bans) |
| `backup.target_dir` | `cmd/groupwarden/storecmds.go` `check --secrets` (must be on a different disk); `cmd/groupwarden/backupcmd.go` `backup` → `internal/backup` `Run`; `groupwarden backup-dir` (the install script lets the backup unit write there) |
| `backup.age_recipient` | `cmd/groupwarden/storecmds.go` `check --secrets` (parsed by `age`); `internal/backup` `Run` (each backup is encrypted to it) |
| `backup.keep` | `internal/backup` `Run` → `prune` (keeps the newest this many backups; other files in the directory are never touched) |
| `evidence.max_attachment_mb` | `internal/pipeline/moderator.go` `evidence` (an attachment over it is recorded by type, name and size only); `internal/action/media.go` `fetchOne` |
| `report.attachment_show_hours` | `internal/telegram/deliver.go` `takeDownDue` (an attachment posted to the admin chat is deleted after it), `takeDown` (the placeholder's text); `internal/telegram/updates.go` `show` (the reply says when the repost comes down) |
| `bans.scope` | `internal/rules/compile.go`, `decide.go` (`Decision.BanIn`) |
| `word_lists` | `internal/rules/compile.go` `addWords` |
| `leet_word_lists` | `internal/rules/compile.go` |
| `never_match` | `internal/rules/compile.go`; `internal/normalise` `Prepare` |
| `allowed_domains` | `internal/rules/compile.go`; `conditions.go` `linkSignals` |
| `rules.min_word_length` | `internal/rules/compile.go` `addWords` |
| `rules.list` (`name`, `action`, `confirmed`, `on`, `when`) | `internal/rules/compile.go` `compileRules`, `decide.go` |
| `communities.<id>` (`name`, `groups`, `mode`, `disable_rules`, `word_lists`) | `internal/rules/compile.go` `compileCommunities`; `internal/pipeline/directory.go` |
| `communities.<id>.retention` | `internal/config/config.go` `RetentionFor` |

## Writing words and rules

- Every word and phrase is in quotes. Matching is whole-word and ignores case, accents, look-alike letters (Unicode UTS #39), invisible characters and letters spaced out with single spaces or dots.
- A `*` may stand at the start or end of a word (`"airdrop*"`, `"*coin"`), never in the middle or on its own.
- Punctuation inside an entry splits it the same way it splits a message: `"1.10"` is the phrase `1 10`.
- A single word shorter than `rules.min_word_length` is refused; a phrase is not.
- A rule's `when:` is one of `all:`, `any:`, `words:` (one list name or several, any of them) or `has:` (built-in signals: `any_link`, `invite_link`, `shortener`, `phone_number`, `contact_card`, `handle`, `money_amount`).
- A rule with `action: delete_remove_ban` acts only when its community is in `enforce` mode and the rule has `confirmed: true`; until then it reports what it would have done.
- `on: push_name` matches the sender's display name on its own; it is never combined with the message.
