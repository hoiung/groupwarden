# Architecture

One Go process on one machine. It is a linked device on the bot's own WhatsApp number, reads every group it is in, and applies the deployer's rules.

```
WhatsApp ──► internal/client/whatsmeow (the only package that imports whatsmeow)
               │ each event is written to the inbox BEFORE WhatsApp gets its ack
               ▼
           internal/store (groupwarden.db: inbox, pauses, status)
               ▼
           internal/pipeline worker ── Moderator.Evaluate
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

           internal/reconcile sweep (at connect and every reconcile interval):
               banned members present ► removed; their join requests ► rejected
```

| Package | Job |
|---|---|
| `cmd/groupwarden` | The CLI: `pair`, `run`, `groups`, `resolve-link`, `check`, `healthcheck`, `corpus test`, `ledger summary`, `ban add\|remove\|list`, `member show\|forget`, `fatal-exit-code` |
| `internal/client` | The `Adapter` interface and message types; `whatsmeow/` is the only implementation |
| `internal/app` | Supervisor: connect, backoff, fatal states, health monitors, reload |
| `internal/pipeline` | Inbox worker, the group directory (communities, members, admins), the moderator, ban enforcement on joins |
| `internal/ledger` | Writes each decision's actions, ban, evidence and report in one transaction; report delivery; crash recovery; retention purge |
| `internal/action` | Fires the outbox: rate limit, circuit breaker, fire-time re-check; saves evidence attachments |
| `internal/reconcile` | The periodic sweep, backing off on WhatsApp's rate limit |
| `internal/config` | YAML 1.2 parsing, schema validation, defaults, the reloadable `Holder` |
| `internal/rules` | Rule compilation and checks, built-in conditions, the decision |
| `internal/normalise` | The matching views of a text and the word/phrase matcher |
| `internal/corpus` | Labelled spam and legit samples, and the corpus test |
| `internal/store` | SQLite: migrations, inbox, ledger, outbox, ban list, evidence, reports, pauses; maintenance of the WhatsApp session store (message-secret purge, `member forget`) |
| `internal/mask` | Masks phone numbers, LIDs and group IDs in logs |
| `schema` | The config schema (embedded in the binary) |

The config keys and the code that reads each one are in [config.md](config.md).

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
| `filippo.io/age` | Checking the backup recipient key (and encrypting backups) | `v1.3.2` | 2026-08-29 | BSD-3-Clause |
| `github.com/mdp/qrterminal/v3` | Pairing QR code in the terminal | `v3.2.1` | 2025-03-19 | MIT |
| `google.golang.org/protobuf` | WhatsApp message types | `v1.36.12` | 2026-08-10 | BSD-3-Clause |
| `golang.org/x/time/rate` | The token bucket on outbound WhatsApp actions | `v0.16.0` | 2026-08-19 | BSD-3-Clause |

Go's standard `regexp` (RE2, linear time) is the only regular-expression engine; deployers cannot supply their own patterns.

The bundled URL-shortener list (`internal/rules/data/shorteners.txt`) is named in `NOTICE` with its source and licence.
