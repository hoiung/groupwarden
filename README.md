# groupwarden

A self-hosted anti-spam moderator for WhatsApp Communities. It sits in every group of your community as a co-admin, reads each new message, and when a message matches your spam rules it deletes the message, removes the sender and bans them from every group of every community you run with it. Every action is reported to a private Telegram admin chat with the full message as proof and a one-tap **[Undo]**.

Spam is defined by rules you write in a config file: a keyword from one of your word lists **plus** a link or contact detail (phone number, contact card, group invite, link shortener), or words from two of your lists together (for example a crypto word and "inbox me"). A keyword on its own never gets anyone removed.

> **Status:** in development — see [Issue #1](https://github.com/hoiung/groupwarden/issues/1).

## Read this first: the risks

- **Terms of service.** WhatsApp's official Business Platform cannot delete messages or remove people in groups it did not create, so groupwarden runs as an **unofficial linked-device client** (via the whatsmeow library). That breaks WhatsApp's Terms of Service. The practical risk is that WhatsApp **bans the bot's number**.
- **Use a dedicated number.** Put a dedicated SIM in a phone, install WhatsApp, and set a **two-step verification PIN**. Open WhatsApp on that phone at least every 14 days, or WhatsApp logs out its linked devices.
- **Keep humans in charge.** Keep at least **2 human admins** in every group and a **human community owner**. The bot is a co-admin, never the only admin and never the owner. If the bot's number is banned, your humans still run the community.
- **One maintainer upstream.** whatsmeow has one main maintainer, and the call that joins a community's groups without an invite link uses an internal whatsmeow API that a library update could break (the bot then asks admins for an invite link instead).

**Not affiliated** with, endorsed by, or sponsored by WhatsApp or Meta. "WhatsApp" is used only to describe what this tool works with; no WhatsApp logo is used.

## Intended use

Moderating communities **you administer**. It is not for bulk messaging, member scraping or exporting member lists, and it sends nothing on WhatsApp except deletes, removals, join-request rejections and group joins.

## Who is responsible for the data

Whoever deploys groupwarden is the **data controller** for the members' data it processes. Templates for a privacy assessment (DPIA), a legitimate-interests assessment, a member notice and a record of processing are in `docs/privacy/`.

## What it cannot see

- **View-once** posts (WhatsApp never delivers them to linked devices).
- Images, videos or files **without a caption** — v1 reads text only, not text inside images.
- Anything posted **before the bot joined** a group, and groups it is **not in**.
- Replies to announcements posted before it joined, or more than 90 days before the reply.
- A pitch **split across two messages** (each message is judged on its own).

## How it works

1. The bot's number joins every group of each configured community (by itself where WhatsApp allows it, otherwise through an invite link an admin sends with `/join`). A human admin promotes it to admin in each group — no bot can promote itself.
2. Each message is saved to a local inbox before it is acknowledged, so a crash never loses one.
3. Your rules decide whether it is spam. If it is, the bot saves an evidence copy (text and any attachment, kept 30 days), deletes the message for everyone, removes the sender and adds them to the ban list.
4. A banned person who rejoins — by link, join request or community join — is removed or rejected. Only an admin can unban them: **[Undo]** in Telegram, `/unban`, or re-adding them by hand.
5. Every action, with the rule and config version that caused it, goes to the Telegram admin chat and the local ledger. Make the bot an admin of that chat with the **Pin messages** right: it pins a list of its commands there. Every day at 12:00 (`daily_check_time`) it also posts that it is alive and working, with the date; no post means look at the node.

Rules live in a separate private config repo. Paste a new spam message into Claude Code and the `spam-intake` skill saves it to your private corpus, tests every rule against the whole corpus, proposes the smallest rule change with before/after counts, and commits it on your yes; the running bot picks it up within minutes. Install the skill with `make install-skill` (it links `.claude/skills/spam-intake` into `~/.claude/skills`).

## Run it

On a machine that stays on: a Linux box, a Windows PC running WSL2, or Docker. [docs/deploy.md](docs/deploy.md) walks through the install (systemd units, an install script that rolls back a bad upgrade, nightly encrypted backups); [docs/runbook.md](docs/runbook.md) says what to do when the bot needs a human; [docs/config.md](docs/config.md) lists every config key.

## Known limits

- WhatsApp never confirms a delete, so the ledger records "requested", never "succeeded".
- Admins can only delete messages up to 2 days old, so after more than 2 days of downtime older spam can no longer be deleted.
- A human admin can demote the bot at any time (the bot notices and alerts).
- One bot number covers all your communities, so a ban of that number pauses moderation everywhere until you re-pair a new number.
- A hijacked account that posts spam is banned like any other; the person asks the admins to be let back in.

## Repository layout

| Path | What |
|---|---|
| `cmd/groupwarden/` | The CLI (`pair`, `run`, `groups`, `resolve-link`, `check`, `healthcheck`, `ban`, `member`, `ledger`, `corpus`, `sync-config`, `schedule`, `backup`, `restore`) |
| `.claude/skills/spam-intake/` | The Claude Code skill that turns a pasted spam message into a tested rule change |
| `internal/` | Client layer, store, pipeline, rules, ledger, actions, Telegram, reconcile, backup |
| `schema/` | JSON Schema for `config.yaml` |
| `examples/` | Example config with English word lists |
| `tests/corpus/` | Synthetic, redacted spam and look-alike samples |
| `deploy/` | systemd units, install script, Dockerfile |
| `docs/` | Deploy guide, runbook, config reference, privacy templates, research |

Runtime state (`data/`, `evidence/`, `backups/`, `secrets/`) is never committed.

## Licence

Source: MIT (see `LICENSE`). Built binaries and images link GPL-3.0 code and are GPL-3.0 — see `NOTICE`.
