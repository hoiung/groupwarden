# Deploying groupwarden

groupwarden runs as one systemd service on one always-on machine (a "node"): a Linux box, or a Windows PC running WSL2. Three units come with it, in `deploy/`:

| Unit | What it does |
|---|---|
| `groupwarden.service` | `groupwarden run`: the bot. Restarts on a crash, but not on exit code 78 (`groupwarden fatal-exit-code`): logged out, banned or client outdated need a human, and restarting into them only hammers WhatsApp |
| `groupwarden-sync.timer` → `groupwarden-sync.service` | `groupwarden sync-config` every `config_sync_minutes`: pulls your private config repo, checks it and swaps it in (see [config.md](config.md)) |
| `groupwarden-backup.timer` → `groupwarden-backup.service` | `groupwarden backup` every night at 03:15: an encrypted copy of `groupwarden.db` on another disk |

Both timers have `Persistent=true`, so a run missed while the node was off happens as soon as it is back. Upgrades and rollback are in [runbook.md](runbook.md).

## What you need

- A dedicated SIM in a phone with WhatsApp installed and a two-step verification PIN set (see the README's risks).
- A Telegram bot token from BotFather, and a private Telegram group for your admins with the bot in it as an admin with the **Pin messages** right. Its chat ID is a negative number; send a message in the group and read it from `https://api.telegram.org/bot<token>/getUpdates`.
- A private git repo for your config (`config.yaml` at its root, plus your corpus), and a read-only deploy key for it.
- An [age](https://age-encryption.org) key pair for the backups. Only the public key (`age1…`) goes on the node, in `backup.age_recipient`; keep the private key somewhere else, or the backups are only as safe as the node.
- A second disk for the backups (`backup.target_dir`). `groupwarden check --secrets` refuses a target on the same disk as the data.
- Go (the version in `go.mod`) and git on the node, for `deploy/install.sh --version`.
- The node's clock in your time zone: the daily check is posted at `daily_check_time` (12:00 by default) in the node's local time. `date` shows it; set it with `sudo timedatectl set-timezone Europe/London` (your zone), or `-e TZ=Europe/London` for Docker. The bot logs the zone it uses when it starts.

## Layout on the node

| Path | What | Owner, mode |
|---|---|---|
| `/usr/local/bin/groupwarden` | The binary (`groupwarden.previous` beside it after an upgrade) | root, 0755 |
| `/etc/systemd/system/groupwarden*` | The units, and the backup unit's drop-in `groupwarden-backup.service.d/target.conf` | root, 0644 |
| `/var/lib/groupwarden` | Home of the `groupwarden` user and the service's state directory: `data_dir` (groupwarden.db, the WhatsApp session, evidence) | groupwarden, 0700 |
| `/var/lib/groupwarden/config-repo` | The sync's clone of your private config repo | groupwarden |
| `/var/lib/groupwarden/config` | The live config directory: a symlink to `config.releases/<commit>`, swapped by the sync. Every unit reads `config/config.yaml` | groupwarden |
| `/var/lib/groupwarden/.ssh/known_hosts` | The git host's key, for the sync's `git pull` | groupwarden, 0600 |
| `/etc/groupwarden/secrets.env` | `TELEGRAM_TOKEN=…` and `TELEGRAM_CHAT_ID=…`; groupwarden refuses the file if group or others can read it | groupwarden, 0600 |
| `/etc/groupwarden/deploy_key` | The read-only deploy key's private half | groupwarden, 0600 |
| `backup.target_dir` (another disk) | `groupwarden-<time>.db.age` files | groupwarden, 0700 |

The units run with systemd's sandboxing (`ProtectSystem=strict`, `ProtectHome=yes`, no capabilities, a system-call filter): the service can write only its state directory, and the backup unit only its target as well (the drop-in), with no network at all.

So the `config.yaml` in your config repo says:

```yaml
data_dir: /var/lib/groupwarden
secrets_file: /etc/groupwarden/secrets.env
deploy_key_file: /etc/groupwarden/deploy_key
corpus_dir: corpus            # read from the config file's directory, so it moves with each synced copy
backup:
  target_dir: /srv/groupwarden-backup   # on another disk
  age_recipient: "age1..."
  keep: 14
```

The config repo also needs its first corpus before the first sync. Every load tests the rules against `corpus/` and refuses one without both spam and legit samples, so the sync would REJECT the commit. Start from the public corpus and check the config from a clone of this repo before you push:

```bash
cp -r tests/corpus <config repo>/corpus
go run ./cmd/groupwarden check --config <config repo>/config.yaml   # must print "OK config v…"
```

The check also fails when your rules miss a spam sample or would delete a legit one. Add your own samples later with the spam-intake skill (`.claude/skills/spam-intake`) or `groupwarden corpus add`.

## Install on a Linux node

Run these as your own user (with sudo) from a clone of this repo. Each step says how to check it.

1. **Install the binary, the user and the units.**

   ```bash
   git fetch --tags
   deploy/install.sh --version <tag or commit>
   ```

   It builds that version from a throwaway clone, creates the `groupwarden` user and installs the units. It ends with "no live config … yet" and "not enabled yet": both are expected at this point.

2. **Secrets, deploy key and the backup disk.**

   ```bash
   sudo install -d -o root -g groupwarden -m 0750 /etc/groupwarden
   sudo install -o groupwarden -g groupwarden -m 0600 /dev/null /etc/groupwarden/secrets.env
   sudoedit /etc/groupwarden/secrets.env        # TELEGRAM_TOKEN=... and TELEGRAM_CHAT_ID=...
   sudo install -o groupwarden -g groupwarden -m 0600 <deploy key file> /etc/groupwarden/deploy_key
   sudo install -d -o groupwarden -g groupwarden -m 0700 /srv/groupwarden-backup   # a directory on the other disk
   ```

3. **Trust the git host and clone the config repo.** Fetch the host's key, then compare its fingerprint with the one the host publishes (for GitHub: [GitHub's SSH key fingerprints](https://docs.github.com/en/authentication/keeping-your-account-and-data-secure/githubs-ssh-key-fingerprints)) before you trust it.

   ```bash
   sudo install -d -o groupwarden -g groupwarden -m 0700 /var/lib/groupwarden /var/lib/groupwarden/.ssh
   ssh-keyscan -t ed25519 github.com > /tmp/host.pub
   ssh-keygen -lf /tmp/host.pub                 # must match the published fingerprint
   sudo install -o groupwarden -g groupwarden -m 0600 /tmp/host.pub /var/lib/groupwarden/.ssh/known_hosts
   sudo -u groupwarden env GIT_SSH_COMMAND="ssh -i /etc/groupwarden/deploy_key -o IdentitiesOnly=yes" \
     git -C /var/lib/groupwarden clone git@github.com:<you>/<config repo>.git config-repo
   ```

   (`git -C` because the `groupwarden` user may not be able to read the directory you are in.)

4. **First sync.** It copies the repo's current commit to `config.releases/` and points `config` at it.

   ```bash
   sudo systemctl start groupwarden-sync.service
   sudo journalctl -u groupwarden-sync.service -n 20   # its result line starts with "ok"
   sudo -u groupwarden groupwarden check --secrets --config /var/lib/groupwarden/config/config.yaml
   ```

   Every line of `check --secrets` must say OK. If the sync says `/var/lib/groupwarden/config is a directory, not the symlink the sync swaps`, an earlier hand-made config is in the way: move it aside (`sudo mv /var/lib/groupwarden/config /var/lib/groupwarden/config.old`) and start the sync again.

5. **Pair the bot phone.**

   ```bash
   sudo -u groupwarden groupwarden pair --config /var/lib/groupwarden/config/config.yaml
   ```

   Scan the QR code from the bot phone (WhatsApp → Linked devices → Link a device), or add `--phone <digits with country code>` to get a code to type in instead.

6. **Install again**, now that the live config exists: this renders the sync timer from `config_sync_minutes` and lets the backup unit write to `backup.target_dir`.

   ```bash
   deploy/install.sh --version <the same tag or commit>
   ```

   Check its line `sync timer: OnCalendar=…; backup unit may write …`.

7. **Enable and start everything.**

   ```bash
   sudo systemctl enable --now groupwarden.service groupwarden-sync.timer groupwarden-backup.timer
   sudo -u groupwarden groupwarden healthcheck --config /var/lib/groupwarden/config/config.yaml
   ```

   `healthcheck` exits 0 once the bot is running, connected, hearing messages, has its config and has reached the admin chat. The admin chat gets a "started" message, the pinned command list and the coverage of each community; `/status` shows the rest.

8. **Test a backup once** rather than waiting for the night: `sudo systemctl start groupwarden-backup.service`, then `sudo ls -l /srv/groupwarden-backup` and `/status` ("Backup: last run …").

Run `deploy/install.sh` again whenever you change `config_sync_minutes` or `backup.target_dir`: those two are baked into the units.

## Windows node running WSL2

The same steps, inside a WSL2 distribution (Ubuntu, say), plus the work below so that WSL runs at boot with nobody logged in and never goes idle. The Windows steps need an administrator PowerShell.

1. **Use the Microsoft Store version of WSL**: `wsl --version` must print a version (if it does not, run `wsl --update`). Systemd and `wsl --mount --vhd` need it.
2. **Turn systemd on** in the distribution's `/etc/wsl.conf`, then run `wsl --shutdown` in PowerShell and open the distribution again; `systemctl is-system-running` must print `running` (or `degraded`):

   ```ini
   [boot]
   systemd=true
   ```

3. **Stop WSL shutting down when idle.** In `%UserProfile%\.wslconfig`:

   ```ini
   [wsl2]
   vmIdleTimeout=-1

   [general]
   instanceIdleTimeout=-1
   ```

   `instanceIdleTimeout` stops WSL shutting the distribution down when the last terminal closes; `vmIdleTimeout` does the same for the virtual machine. Microsoft documents `vmIdleTimeout` for Windows 11 only, so on Windows 10 the boot task below is what keeps WSL alive: its `wsl.exe` never exits.
4. **Keep everything on the Linux filesystem.** `data_dir` and the config must be inside the distribution (`/var/lib/groupwarden`, as above), never under `/mnt/c`: groupwarden refuses a Windows drive for its database.
5. **A backup disk WSL can mount.** A Windows drive (`/mnt/d`) is refused for the same reason, so put an ext4 virtual disk on the second physical drive and have the boot task attach it. Once, in an administrator PowerShell and then the distribution:

   ```powershell
   mkdir D:\groupwarden
   Set-Content $env:TEMP\vdisk.txt "create vdisk file=D:\groupwarden\backup.vhdx maximum=20480 type=expandable"
   diskpart /s $env:TEMP\vdisk.txt
   wsl --mount --vhd D:\groupwarden\backup.vhdx --bare
   ```

   ```bash
   lsblk                                   # the new 20G disk, for example /dev/sde
   sudo mkfs.ext4 /dev/sde
   ```

   ```powershell
   wsl --unmount D:\groupwarden\backup.vhdx
   wsl --mount --vhd D:\groupwarden\backup.vhdx --name gwbackup
   ```

   ```bash
   sudo install -d -o groupwarden -g groupwarden -m 0700 /mnt/wsl/gwbackup/groupwarden
   ```

   `backup.target_dir` is then `/mnt/wsl/gwbackup/groupwarden`. The `.vhdx` file must sit on a different physical drive from the one holding WSL, or a disk failure takes both.
6. **Start WSL at boot, whether or not anyone logs in.** Save this as `C:\groupwarden\wsl-boot.ps1` (with your distribution's name):

   ```powershell
   # Attach the backup disk, then keep WSL (and systemd, and groupwarden) running.
   wsl.exe --mount --vhd D:\groupwarden\backup.vhdx --name gwbackup
   wsl.exe -d Ubuntu --exec /bin/sleep infinity
   ```

   and register it as a task that runs at startup as your Windows user, with highest privileges (`wsl --mount` needs them), with no time limit (Task Scheduler's default stops a task after 3 days), restarting if it stops:

   ```powershell
   $action   = New-ScheduledTaskAction -Execute "powershell.exe" -Argument "-NoProfile -ExecutionPolicy Bypass -File C:\groupwarden\wsl-boot.ps1"
   $trigger  = New-ScheduledTaskTrigger -AtStartup
   $settings = New-ScheduledTaskSettingsSet -ExecutionTimeLimit ([TimeSpan]::Zero) -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -RestartCount 999 -RestartInterval (New-TimeSpan -Minutes 1)
   Register-ScheduledTask -TaskName "groupwarden WSL" -Action $action -Trigger $trigger -Settings $settings -RunLevel Highest -User "$env:USERDOMAIN\$env:USERNAME" -Password (Read-Host "Windows password for $env:USERNAME")
   ```

7. **Never sleep on mains power, and closing the lid does nothing:**

   ```powershell
   powercfg /change standby-timeout-ac 0
   powercfg /change hibernate-timeout-ac 0
   powercfg /setacvalueindex SCHEME_CURRENT SUB_BUTTONS LIDACTION 0
   powercfg /setactive SCHEME_CURRENT
   ```

8. **Prove it**: restart Windows, do not log in, and check `/status` in the admin chat from your phone; then log in, close every WSL terminal, wait 10 minutes and check `/status` again. Both should show the bot connected with the same config version. Windows Update restarts the PC from time to time; the boot task brings everything back each time.

## Docker

For adopters who run containers rather than systemd. `deploy/Dockerfile` builds a static binary into a distroless image that runs as a non-root user and has a health check (`groupwarden healthcheck`).

```bash
docker build -f deploy/Dockerfile -t groupwarden .
```

Put `config.yaml` (with `data_dir: /data` and `secrets_file: /config/secrets.env`) and the secrets file (owned by the container's user, uid 65532, mode 0600) in a directory mounted at `/config`, and keep the data in a named volume at `/data`:

```bash
docker volume create groupwarden-data
docker run -it --rm -v groupwarden-data:/data -v /path/to/config:/config:ro groupwarden pair
docker run -d --name groupwarden --restart on-failure -e TZ=Europe/London -v groupwarden-data:/data -v /path/to/config:/config:ro groupwarden
```

`pair` takes the same lock as `run`, so pair before you start the bot. The image has no git and no timers, and it sets `GROUPWARDEN_NO_SYNC_TIMER=1` so the bot does not alert that the config sync is overdue: reload a changed config with `docker kill --signal HUP groupwarden`. Run the backup from the host's scheduler (the bot still alerts when it has not run for two days), into a directory the container's user can write, with `backup.target_dir: /backup`:

```bash
sudo install -d -o 65532 -g 65532 -m 0700 /path/to/backups
docker run --rm -v groupwarden-data:/data -v /path/to/config:/config:ro -v /path/to/backups:/backup groupwarden backup
```

`--restart on-failure` also restarts after exit code 78, which needs a human; check the admin chat before you restart a stopped container.
