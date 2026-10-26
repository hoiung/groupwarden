# groupwarden

A self-hosted anti-spam moderator for WhatsApp Communities. It sits in every group of your community as a co-admin, reads each new message, and when a message matches your spam rules it deletes the message, removes the sender and bans them from every group of every community you run with it. Every action is reported to a private Telegram admin chat with the full message as proof and a one-tap **[Undo]**.

Spam is defined by rules you write in a config file: a keyword from one of your word lists **plus** a link or contact detail (phone number, contact card, group invite, link shortener), or two different words from two of your lists together (for example a crypto word and "inbox me"). A keyword on its own never gets anyone removed, even a word you put in two lists.

> **Status:** in development — see [Issue #1](https://github.com/hoiung/groupwarden/issues/1).

## Read this first: the risks

- **Terms of service.** WhatsApp's official Business Platform cannot delete messages or remove people in groups it did not create, so groupwarden runs as an **unofficial linked-device client** (via the whatsmeow library). That breaks WhatsApp's Terms of Service. The practical risk is that WhatsApp **bans the bot's number**.
- **Use a dedicated number.** Put a dedicated SIM in a phone, install WhatsApp, and set a **two-step verification PIN**. Open WhatsApp on that phone at least every 14 days, or WhatsApp logs out its linked devices.
- **Keep humans in charge.** Keep at least **2 human admins** in every group and a **human community owner**. The bot is a co-admin, never the only admin and never the owner. If the bot's number is banned, your humans still run the community.
- **One maintainer upstream.** whatsmeow has one main maintainer, and the call that joins a community's groups without an invite link uses an internal whatsmeow API that a library update could break (the bot then asks admins for an invite link instead).

**Not affiliated** with, endorsed by, or sponsored by WhatsApp or Meta. "WhatsApp" is used only to describe what this tool works with; no WhatsApp logo is used.

## Intended use

Moderating communities **you administer**. It is not for bulk messaging, member scraping or exporting member lists, and it sends nothing on WhatsApp except deletes, removals, join-request rejections and group joins.

## What it cannot see

- **View-once** posts (WhatsApp never delivers them to linked devices).
- Images, videos or files **without a caption** — v1 reads text only, not text inside images.
- Anything posted **before the bot joined** a group, and groups it is **not in**.
- Replies to announcements posted before it joined, or more than 90 days before the reply.
- A pitch **split across two messages** (each message is judged on its own).

## How it works

1. An admin switches a community or a single group on: `/enable <invite link>` in the admin chat (a community link covers the whole community; a group link covers that group alone, so a friend can share one public group without the rest), or a `communities:` entry in the config file. `/disable <invite link>` (or the name `/status` shows, once a link no longer works) switches off what `/enable` switched on. The bot's number joins every group of what is on: through the link `/enable` was sent with, and a community's other groups by itself where WhatsApp allows it, otherwise when an admin sends `/enable` with that group's own link. A human admin promotes it to admin in each group — no bot can promote itself.
2. Each message is saved to a local inbox before it is acknowledged, so a crash never loses one.
3. Your rules decide whether it is spam. If it is, the bot saves an evidence copy (text and any attachment, kept 30 days), deletes the message for everyone, removes the sender and adds them to the ban list.
4. A banned person who rejoins — by link, join request or community join — is removed or rejected until they are unbanned: **[Undo]** in Telegram, `/unban`, or an admin re-adding them by hand. Without an `acl:` section in the config only admins of the admin chat can use the first two; with one, everyone in that chat can ([Admin-chat access](docs/config.md#admin-chat-access-acl)). A spammer the bot posted no report about is banned by number: `/ban <number>`.
5. Every action, with the rule and config version that caused it, goes to the Telegram admin chat and the local ledger. Make the bot an admin of that chat with the **Pin messages** right: it pins a list of its commands there. Every day at 12:00 (`daily_check_time`) it also posts that it is alive and working, with the date; no post means look at the node.

Rules live in a separate private config repo. Paste a new spam message into Claude Code and the `spam-intake` skill saves it to your private corpus, tests every rule against the whole corpus, proposes the smallest rule change with before/after counts, and commits it on your yes; the running bot picks it up within minutes. Install the skill with `make install-skill` (it links `.claude/skills/spam-intake` into `~/.claude/skills`).

## Admin chat commands

The bot pins this list in the admin chat, and typing `/` there shows it:

| Command | What it does |
|---|---|
| `/status` | what the bot covers in each community, pauses, config version and timers |
| `/pause`, `/resume` | stop every action, deletes included; end every pause (queued actions are checked again first) |
| `/reload` | load the config again (a rejected config leaves the old one running) |
| `/enable <invite link>`, `/disable <invite link or name>` | moderate what the link points to (a community or one group), joining it if needed; stop moderating it |
| `/ban <number>` | ban a phone number (with its country code) everywhere, for a spammer the bot posted no report about |
| `/ban` as a reply to a "would have acted" report | delete that post, remove and ban its sender |
| `/unban <number>`, `/unban <report number>` | unban that phone number, or the person in that report, everywhere (nobody is added back to a group) |

`/ban <number>` sweeps at once, so the person is removed from every group the bot finds them in, and deletes their posts the bot still has a record of (the last 72 hours; one older than `act_on_replay_max_age` cannot be deleted, and the reply counts it). In a community in shadow mode it does neither: the sweep only reports the person there, and the reply counts their posts there as left. A post they send after the ban, before the sweep removes them, is deleted when it arrives. `/ban` as a reply, [Ban] and [Add to ban list] do the same in a community in enforce mode; in shadow mode the ban is only recorded. [docs/config.md](docs/config.md#banning-from-the-admin-chat) has the details, and who may use what.

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
| `cmd/groupwarden/` | The CLI (`pair`, `run`, `groups`, `resolve-link`, `check`, `healthcheck`, `ban`, `member`, `ledger`, `corpus`, `sync-config`, `schedule`, `backup`, `backup-failed`, `backup-dir`, `restore`, `fatal-exit-code`) |
| `.claude/skills/spam-intake/` | The Claude Code skill that turns a pasted spam message into a tested rule change |
| `internal/` | Client layer, store, pipeline, rules, ledger, actions, Telegram, reconcile, backup |
| `schema/` | JSON Schema for `config.yaml` |
| `examples/` | Example config with English word lists |
| `tests/corpus/` | Synthetic, redacted spam and look-alike samples |
| `deploy/` | systemd units, install script, Dockerfile |
| `docs/` | Deploy guide, runbook, config reference, [architecture](docs/architecture.md) (how the packages fit together), research |

Runtime state (`data/`, `evidence/`, `backups/`, `secrets/`) is never committed.

## Licence

Source: MIT (see `LICENSE`). Built binaries and images link GPL-3.0 code and are GPL-3.0 — see `NOTICE`.
