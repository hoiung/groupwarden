# Architecture

One Go process on one machine. It is a linked device on the bot's own WhatsApp number, reads every group it is in, and applies the deployer's rules.

```
WhatsApp ──► internal/client/whatsmeow (the only package that imports whatsmeow)
               │ each event is written to the inbox BEFORE WhatsApp gets its ack
               ▼
           internal/store (groupwarden.db: inbox, pauses, status)
               ▼
           internal/pipeline worker ── Moderator.Decide
               │ fields the sender wrote (never the quoted message; @mentions masked)
               ▼
           internal/rules (compiled from internal/config; swapped whole on reload)
               │ internal/normalise: look-alike letters, invisible characters,
               │ spaced letters, digit swaps, never-match phrases, whole words
               ▼
           decision: log, or delete + remove + ban everywhere
               │ internal/ledger: ONE transaction writes the action rows (intended),
               │ queues them in the outbox, adds the ban, keeps the evidence copy
               │ and stores the admin report
               ▼
           internal/action executor: token bucket ► circuit breaker ►
               re-check at fire time ► WhatsApp call ► requested / failed /
               already_gone (a delete is never confirmed, so never "succeeded")

           internal/reconcile sweep (at connect, every reconcile interval, and
           when the bot joins a group; the group list is read from WhatsApp first,
           and a read that fails is tried again every 30 seconds; a sweep waits
           until the list has loaded and, after a connect, until the worker has
           decided everything WhatsApp held while the bot was offline):
               coverage per group (absent / not admin / covered) ► compared with
               what the admins were told ► changes reported; absent linked groups
               ► joined by the bot itself, else /join asked for; a refused
               admin-only call ► group marked not covered at once
               banned members present ► removed; their join requests ► rejected

           internal/telegram: the stored reports and alerts ► the private admin
               chat (≤ 20 posts a minute, priority first); admins' buttons and
               commands ► internal/app Controls ► internal/pipeline Admin

           deploy/groupwarden-sync.timer ► groupwarden sync-config
               (internal/configsync): git pull the private config repo ► the
               same checks as a reload ► swap the live config directory to a
               copy of that commit ► SIGHUP to run ► "config v<hash> loaded"

           deploy/groupwarden-backup.timer ► groupwarden backup (internal/backup):
               snapshot of groupwarden.db ► age-encrypted to backup.target_dir
               (another disk) ► keep the newest backup.keep; `restore` puts one
               back with every action paused

           internal/app health rule (running, connected, not deaf, config
               loaded, admin chat reached) ► `healthcheck` (install script,
               Docker), the heartbeat_url ping every 5 minutes and the daily
               check posted in the admin chat at daily_check_time
```

| Package | Job |
|---|---|
| `cmd/groupwarden` | The CLI: `pair`, `run`, `groups`, `resolve-link`, `check`, `healthcheck`, `corpus test\|add`, `ledger summary`, `ban add\|remove\|list`, `member show\|forget`, `sync-config`, `schedule sync`, `backup`, `backup-dir`, `restore`, `fatal-exit-code` |
| `internal/client` | The `Adapter` interface and message types; `whatsmeow/` is the only implementation |
| `internal/app` | Supervisor: connect, backoff, fatal states, health monitors, reload, lifecycle messages, overdue-timer and phone reminders, the daily check; the admin chat's controls; the data-dir lock (it names the holding process and command, so the config sync signals only `run`); `Build`, the one wiring of the bot's parts (`run` and the log test both use it); the health rule `healthcheck`, the heartbeat ping and the daily check share |
| `internal/backup` | The nightly backup (snapshot, age encryption, keep the newest `backup.keep`, crash leftovers removed) and `restore` (refuses an existing database, a wrong key or a file that is not a groupwarden backup) |
| `internal/pipeline` | Inbox worker (it decides nothing until the group directory has loaded once), the group directory (communities, members, admins: from WhatsApp's group list, kept current by membership, admin and join events, which a list read already on its way cannot undo), the moderator, ban enforcement on joins, what the admin chat's [Undo] / [Ban] / [Add to ban list] write |
| `internal/ledger` | Writes each decision's actions, ban, evidence and report in one transaction; crash recovery; retention purge |
| `internal/telegram` | The admin chat: report and alert delivery from the store (rate limit, priority, digests, attachments, text removed after the evidence window), buttons and commands from admins of that chat only, the command list pinned there and set as its "/" menu |
| `internal/alert` | Alert kinds and which of them are priority |
| `internal/action` | Fires the outbox: rate limit, circuit breaker, fire-time re-check; saves evidence attachments |
| `internal/reconcile` | The periodic sweep, backing off on WhatsApp's rate limit: group coverage and its reports (new groups, lost groups, fewer than 2 human admins), joining absent linked groups, banned members present, join requests |
| `internal/config` | YAML 1.2 parsing, schema validation, defaults, the reloadable `Holder` |
| `internal/rules` | Rule compilation and checks, built-in conditions, the decision |
| `internal/normalise` | The matching views of a text and the word/phrase matcher |
| `internal/corpus` | Labelled spam and legit samples; the corpus test (per-rule hits, the closest rule for missed spam, the rules behind a legit false hit); `corpus add` (redaction that keeps a pattern's shape, or full redaction with `--public`; dedupe; a new private corpus seeded from the public legit set) |
| `internal/configsync` | The config sync: pull with the deploy key and a time limit, check, swap the live config directory's symlink, signal `run`; one alert per failure episode |
| `internal/store` | SQLite: migrations, inbox, ledger, outbox, ban list, evidence, reports, pauses, group coverage; maintenance of the WhatsApp session store (message-secret purge, `member forget`) |
| `internal/mask` | Masks phone numbers, LIDs and group IDs in logs and reports (a digit run touching a letter, such as a config hash, is left alone) |
| `schema` | The config schema (embedded in the binary) |

The config keys and the code that reads each one are in [config.md](config.md). How it runs on a node (systemd units, the install script, WSL2, Docker) is in [deploy.md](deploy.md), and what to do when it needs a human in [runbook.md](runbook.md).

## Libraries

Maintained libraries are used instead of hand-written code wherever one fits. Every licence below is compatible with GPL-3.0, which the built binary is under (see `NOTICE`). Versions are the ones pinned in `go.mod`; "released" is the publish date of that version (for a pseudo-version, its commit date). All were the newest available when added (`go list -m -u`).

| Library | Used for | Version | Released | Licence |
|---|---|---|---|---|
| `go.mau.fi/whatsmeow` | WhatsApp Web multi-device client | `v0.0.0-20261005195255-6bb48c0f1ff0` | 2026-10-05 | MPL-2.0 |
| `go.mau.fi/libsignal` (via whatsmeow) | Signal protocol | `v0.2.2` | 2026-05-29 | GPL-3.0 (why the binary is GPL-3.0) |
| `modernc.org/sqlite` | SQLite without cgo | `v1.60.1` | 2026-09-29 | BSD-3-Clause |
| `go.yaml.in/yaml/v3` | YAML parsing (node tree; groupwarden applies YAML 1.2 core typing itself) | `v3.0.5` | 2026-07-26 | MIT and Apache-2.0 |
| `github.com/santhosh-tekuri/jsonschema/v6` | JSON Schema (draft 2020-12) validation of the config | `v6.0.3` | 2026-06-28 | Apache-2.0 |
| `golang.org/x/text` | NFKC, accent folding, case folding | `v0.42.0` | 2026-09-08 | BSD-3-Clause |
| `github.com/eskriett/confusables` | Unicode UTS #39 confusable skeletons | `v0.0.0-20250910043846-220432c5bd73` | 2025-09-10 | MIT |
| `mvdan.cc/xurls/v2` | Finding links with or without `https://` | `v2.6.0` | 2025-01-02 | BSD-3-Clause |
| `golang.org/x/net/publicsuffix` | Registrable domain for `allowed_domains` | `v0.59.0` | 2026-09-08 | BSD-3-Clause |
| `filippo.io/age` | Checking the backup recipient key, encrypting backups and decrypting them on restore | `v1.3.2` | 2026-08-29 | BSD-3-Clause |
| `github.com/mdp/qrterminal/v3` | Pairing QR code in the terminal | `v3.2.1` | 2025-03-19 | MIT |
| `google.golang.org/protobuf` | WhatsApp message types | `v1.36.12` | 2026-08-10 | BSD-3-Clause |
| `golang.org/x/time/rate` | The token bucket on outbound WhatsApp actions and on admin-chat posts | `v0.16.0` | 2026-08-19 | BSD-3-Clause |
| `github.com/go-telegram/bot` | The Telegram Bot API client for the admin chat (no dependencies of its own). Not chosen: `go-telegram-bot-api/v5` (last release 2021-12), `telebot.v4` (beta), `telego` (more dependencies) | `v1.27.0` | 2026-09-11 | MIT |

Go's standard `regexp` (RE2, linear time) is the only regular-expression engine; deployers cannot supply their own patterns.

The bundled URL-shortener list (`internal/rules/data/shorteners.txt`) is named in `NOTICE` with its source and licence.
