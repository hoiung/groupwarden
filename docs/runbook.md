# Runbook

What to do when groupwarden asks for a human. Paths are the node layout in [deploy.md](deploy.md); `CFG` below is `/var/lib/groupwarden/config/config.yaml`.

## Where to look

- **The daily check**: every day at `daily_check_time` (12:00 by default, the node's local time) the bot posts "Daily check, <date>: groupwarden is alive and working." in the admin chat, or names what is not working. See [No daily check](#no-daily-check).
- **The admin chat**: every alert lands there. `/status` shows the connection, pauses, the config version, the last config sync and backup, the ban list and queue sizes, and coverage per community.
- **`sudo -u groupwarden groupwarden healthcheck --config CFG`**: exit 0 only when the bot is running, connected, hearing messages, has its config and has reached the admin chat; otherwise one line says which part is not.
- **`journalctl -u groupwarden.service`** (and `-u groupwarden-sync.service`, `-u groupwarden-backup.service`): structured JSON logs. Phone numbers and IDs are masked and message text is only logged at debug level.

## No daily check

No daily check by a quarter past the time means the bot is not running, or the node is off, asleep or offline: the post comes from the running bot, so nothing else can send it. Look at the node:

1. Is it on and online? On a Windows node: is the laptop on, on mains power, and is WSL up (`wsl -l -v` shows the distro Running)?
2. `systemctl status groupwarden`: if it stopped with exit code 78, see the next section; otherwise `sudo systemctl start groupwarden` and read `journalctl -u groupwarden.service` for why it stopped.
3. `sudo -u groupwarden groupwarden healthcheck --config CFG`.

When the post says "alive but not working fully", it names each part that is not working (WhatsApp not connected, no messages arriving, no config loaded, or the admin chat refusing the bot): the alert for that part, and `healthcheck`, say what to do. A bot started after the time posts that day within a minute; a day it was down for is not posted afterwards.

## The bot stopped and is not restarting (exit code 78)

Exit 78 means WhatsApp needs a human, so systemd does not restart it. The admin chat got a priority alert with WhatsApp's reason and one of these next steps. First look at WhatsApp on the bot phone: if it says the number is banned, go to [Bot number banned](#bot-number-banned) whatever the alert says.

| Alert says | Do this |
|---|---|
| the bot was unlinked | [Re-pair](#re-pair) |
| another copy is using this session | Find and stop the other copy (another node, an old container), then `sudo systemctl start groupwarden` |
| WhatsApp rejected this client version | [Update whatsmeow](#update-whatsmeow--client-outdated) |
| the connection token could not be refreshed | `sudo systemctl start groupwarden`; if it happens again, [re-pair](#re-pair) |
| WhatsApp refused the connection | Check the bot phone has signal and WhatsApp works there, then `sudo systemctl start groupwarden` |

## Re-pair

1. `sudo systemctl stop groupwarden`
2. `sudo -u groupwarden groupwarden pair --config CFG` and link the bot phone again (WhatsApp → Linked devices → Link a device). If it says "already paired", follow the message: unlink that device on the bot phone and move `whatsmeow.db` (and any `whatsmeow.db-wal` and `-shm` beside it) out of `/var/lib/groupwarden`.
3. `sudo systemctl start groupwarden`, then `/status`.

The ban list, ledger and config are in `groupwarden.db` and are untouched by a re-pair.

## The 14-day phone rule

WhatsApp unlinks every linked device when the phone itself has not used WhatsApp for 14 days, and the bot stops until it is paired again. Open WhatsApp on the bot phone at least once a week. The bot asks for it: a weekly reminder with **[Done]** in the admin chat, and a priority alert on day 10 without **[Done]**. Keep the phone charged, on Wi-Fi or mobile data, and with WhatsApp allowed to run in the background.

## Keep the SIM active

The number belongs to the SIM. If the carrier withdraws the number for lack of use, someone else can be given it and register WhatsApp on it, which takes the bot's account with it. Check your carrier's rule for how often the SIM must be used (a text or a top-up, usually every few months) and put a reminder in a calendar. A pay-as-you-go SIM with no plan is enough; the bot needs no mobile data on the phone if the phone has Wi-Fi.

## Moving to a new host

1. On the old host: `sudo systemctl disable --now groupwarden.service groupwarden-sync.timer groupwarden-backup.timer`.
2. On the bot phone, **unlink the old linked device** (WhatsApp → Linked devices). Two copies on one session take it from each other until WhatsApp logs both out.
3. Install the new host from [deploy.md](deploy.md). To keep the ban list and ledger, [restore](#restore-a-backup) the latest backup there before you enable the service, then pair.

## Update whatsmeow / client outdated

WhatsApp retires old client versions; the bot then exits with "WhatsApp rejected this client version". whatsmeow has no releases, only commits, so groupwarden pins one pseudo-version in `go.mod` (its last 12 characters are the commit).

1. Back up first: `sudo systemctl start groupwarden-backup.service`.
2. See what is newer: `go list -m -u go.mau.fi/whatsmeow`.
3. **Read the diff** between the pinned commit and the new one: `https://github.com/tulir/whatsmeow/compare/<pinned commit>...<new commit>`. Look for changes to the calls groupwarden uses (message revoke, group participants, join requests, community groups, the store).
4. Bump the pin: `go get go.mau.fi/whatsmeow@<new commit>`, then `go mod tidy`.
5. Test: `go test -race ./...`, then the corpus test against your own rules: `groupwarden corpus test --config <your config> --corpus <your corpus>`.
6. Commit (and update the version row in `docs/architecture.md`), then install on the node: `deploy/install.sh --version <that commit>`.
7. Check `/status` in the admin chat: connected, the right config version.

## Rollback

`deploy/install.sh` keeps the binary it replaces as `/usr/local/bin/groupwarden.previous`. After an upgrade it restarts the bot and waits up to 3 minutes for `healthcheck`; when that fails it puts the previous binary and units back, restarts and checks again, and says `ROLLED BACK`.

To roll back by hand (the new version is healthy but wrong):

```bash
deploy/install.sh --version <the previous tag or commit>
# or, without building:
sudo cp -p /usr/local/bin/groupwarden.previous /usr/local/bin/groupwarden && sudo systemctl restart groupwarden
```

If the newer version changed the database, the older binary refuses to start ("groupwarden.db is at schema version N but this binary knows only M"): [restore](#restore-a-backup) the backup you took before the upgrade.

## Bot number banned

The bot exits (78) and every group is unmoderated until a new number is paired. Your human admins and the human community owner still run the community; that is why every group needs at least 2 human admins.

1. The admins moderate by hand meanwhile.
2. Appeal from the bot phone if WhatsApp offers it.
3. WhatsApp's Terms say someone whose account was banned must not create another account without WhatsApp's permission. Using a new number for the bot is your decision and your risk.
4. With a new SIM: move `whatsmeow.db` (and its `-wal` and `-shm`) aside, `pair` the new phone, add it to every group and the community, and have a human admin make it an admin everywhere. `/status` lists what is still missing.

A temporary ban is different: the alert says "WhatsApp temporarily banned the bot number" and when it ends. The bot stays disconnected until then and reconnects by itself with removals and bans **paused** (deletes continue) until an admin presses **[Resume]**. Bans usually follow sending too much: look at how many actions it took before the ban.

## Restore a backup

The nightly backup is an age-encrypted copy of `groupwarden.db` only, never the WhatsApp session (pair again after a loss). A restored bot starts with **every action paused**.

1. `sudo systemctl stop groupwarden`
2. Move the current database aside: `sudo -u groupwarden mv /var/lib/groupwarden/groupwarden.db /var/lib/groupwarden/groupwarden.db.old` (and `groupwarden.db-wal` and `-shm` if they exist).
3. Copy the age **private** key to the node for the restore only: `sudo install -o groupwarden -m 0600 <key file> /var/lib/groupwarden/restore.key`
4. `sudo -u groupwarden groupwarden restore --config CFG --identity /var/lib/groupwarden/restore.key <backup file>` (a `groupwarden-<time>.db.age` from `backup.target_dir`).
5. Delete the key: `sudo rm /var/lib/groupwarden/restore.key`
6. `sudo systemctl start groupwarden`. Check `/status`: it shows the restore pause. Press **[Resume]** once you have checked coverage and the config version; queued actions are checked again before they run.

`restore` refuses to run while the bot runs, over an existing database, with the wrong key, and on a file that is not a groupwarden backup.

## The database cannot be written

**"groupwarden.db cannot be written (…): every action is PAUSED until an admin resumes"**: the data disk is full or read-only. Deletes, removals and bans stop at once. Alerts and the daily check still reach the admin chat, sent directly without a record (each once), and the journal logs every alert as it is raised (`alert raised`). Queued reports wait: the bot posts nothing again while it cannot record what it posted, and delivery carries on by itself once the disk takes writes again.

1. `df -h /var/lib/groupwarden` and `journalctl -u groupwarden.service` (the SQLite error is in the log).
2. Free space on that disk, or bring it back read-write.
3. Type `/resume` in the admin chat (the alert has no button: it was sent around the store). `/status` then shows no storage pause; queued actions are checked again before they run.

## Alerts about timers and backups

- **"The config sync timer has not run for …"** or **"The backup timer has not run for …"**: the systemd timer stopped. Check `systemctl list-timers 'groupwarden*'` and `systemctl status groupwarden-sync.service groupwarden-backup.service`. On a Windows node, check that WSL is up and, for the backup, that the backup disk is mounted (`ls /mnt/wsl/gwbackup`); the boot task mounts it.
- **"The nightly backup FAILED: …"**: the reason is in the alert and in `/status`. Usual causes: the target disk is missing or full, or `backup.target_dir` changed without running `deploy/install.sh` again (the backup unit can write only the directory it was given). Earlier backups are kept; it tries again the next night, or now with `sudo systemctl start groupwarden-backup.service`. You get one alert when backups start failing, not one every night.
- **"The config sync FAILED"** or **"Config commit … was REJECTED"**: the bot keeps its current config. Fix the network or the deploy key, or fix the config repo; the next sync tries again.

## Confirming a watch-only rule

A rule added later starts with `confirmed: false`: each match is only reported in the admin chat as "would have acted", with **[Ban]** to act on that one post by hand. Once its reports show only spam:

1. In the private config repo, set `confirmed: true` on that rule.
2. Run the corpus test (`groupwarden corpus test --config config.yaml --corpus corpus`); it must catch every spam sample and no legit one.
3. Commit and push. Within `config_sync_minutes` the admin chat says `config v<hash> loaded`, and `/status` shows that version. From then on the rule deletes, removes and bans.

If its reports include a real member's post, do not confirm it: change the rule (or add the post to the corpus as legit with the `spam-intake` skill) and watch again.
