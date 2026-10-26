# What groupwarden.db keeps about members

The bot's database (`data_dir/groupwarden.db`, `internal/store`) holds what it needs to moderate, for as long as the table below says. Retention runs every hour (`internal/ledger/purge.go`, and the inbox worker for `seen`). `groupwarden member forget <lid|phone>` deletes one person's evidence copies, action-log rows and queued inbox rows, empties `seen.sender` and `seen.sender_alt` for them and has their message text taken off the admin chat's reports; an active ban, a ban a pause holds and an action still queued are kept (`internal/store/member.go` `ForgetMember`).

| Table | What it holds about a member | How long |
|---|---|---|
| `inbox` | a delivered event, the message text included | until the worker decides it: the decision deletes the row (`internal/store/inbox.go` `Decide`) |
| `seen` | one row per decided delivery: the group, the message ID, its server time, when it was decided, and `sender` and `sender_alt`: who wrote a post of its own, by the address the group knew them by (a LID or a phone JID) and their other address when WhatsApp gave it (both empty for an edit, a membership change and every row written before store migration 12). Never the text | 72 hours after the decision, the same as before `sender` and `sender_alt` were added (`internal/pipeline/worker.go` `seenRetention`, `internal/store/inbox.go` `PurgeSeen`) |
| `evidence` (and its attachment files under `data_dir/evidence`) | a copy of a post a rule matched: its text, sender, display name and attachment | `retention.evidence_days` (30 by default; the purge keeps the longest any community sets) |
| `ledger`, `reports` | every action, naming its target by LID or phone JID; a report names the member it is about but holds no message text | `retention.action_log_months` (12 by default, the longest any community sets); a queued action and a ban a pause holds stay until they are settled |
| `bans` | the ban list: a banned member's LID and phone JID | until the ban is lifted |
| `tg_presses` | who pressed a report's button or sent its command (Telegram user ID and name) | as long as the report |

`seen.sender` and `seen.sender_alt` are there so that every `/ban` from the admin chat can delete the banned person's posts the bot still has a record of ([Banning from the admin chat](../config.md#banning-from-the-admin-chat)). `member forget` empties both for that person and leaves the rest of the row, so a redelivered message is still ignored.
