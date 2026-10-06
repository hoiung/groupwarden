---
name: spam-intake
description: Turn a pasted WhatsApp spam message (or a wrongly deleted legit one) into a groupwarden corpus sample and the smallest rule change that handles it, tested against every saved sample, then commit it to the user's PRIVATE config repo. Use when the user pastes a message and says it is spam the bot missed, or a message the bot should not have deleted.
---

# spam-intake: paste a message, get a tested rule change

The pasted message goes into the user's PRIVATE corpus, never into the public
groupwarden repo. Every proposed change is tested with `groupwarden corpus test`
against all saved spam and legit samples, shown to the user with before/after
counts, and applied only on the user's yes.

## 1. Find the tools and the private config

1. The binary: `command -v groupwarden`. If it is not installed, build it from
   the clone this skill is linked from (keep that clone at the version the bot
   runs, so the rules behave the same):
   `repo=$(dirname "$(dirname "$(dirname "$(readlink -f ~/.claude/skills/spam-intake)")")")`,
   then `(cd "$repo" && go build -o "$HOME/.cache/groupwarden/groupwarden" ./cmd/groupwarden)`.
2. The config: `$GROUPWARDEN_CONFIG` if set, otherwise ask the user once for the
   path of `config.yaml` in their private config repo clone. Its directory must
   be inside a git repo (`git -C <dir> rev-parse --show-toplevel`).
3. Safety checks before writing anything; stop and tell the user if one fails:
   - the config repo is not the public groupwarden repo, nor inside it
     (compare `git -C <dir> rev-parse --show-toplevel` with `$repo`);
   - when `gh` is available, `gh repo view <origin> --json visibility --jq .visibility`
     prints `PRIVATE`.
4. The corpus: the config's `corpus_dir`, relative to the config file's
   directory when it is not absolute. If `corpus_dir` is not set, propose adding
   `corpus_dir: corpus` (in the same yes/no as the rule change below).

## 2. Save the sample

Ask what kind of post it was if it is not obvious (`text`, `image-caption`,
`poll`, `contact`, `invite`, `event`) and the sender's display name if the user
has it. Then pipe the pasted text in exactly as given, with a heredoc delimiter
that does not occur in it:

```bash
groupwarden corpus add --label spam --corpus <corpus> --type <type> \
  [--push-name '<display name>'] --note '<one line: why it is spam>' <<'GW_SAMPLE_END'
<pasted message>
GW_SAMPLE_END
```

`corpus add` needs no `--config`. It masks the personal part of phone numbers,
invite codes and @-mentions but keeps their shape, so the rules still see a
phone number or an invite link. On a new corpus it first copies the public
synthetic legit samples in (`SEEDED ...`), so a change is never measured against
zero legit messages. It prints `ADDED <file>`, or `DUPLICATE <file>` when the
same message is already saved (say so and go on to step 3). If it says the
message is already labelled the other way, stop and ask the user which label is
right.

For a message the bot should NOT have deleted, use `--label legit` and go to
step 5 with "false hit" in place of "missed".

## 3. Test before

```bash
groupwarden corpus test --config <config> --corpus <corpus>
```

Keep the `TOTAL` line and the `RULE` lines as the "before" counts. For the new
sample find its line:

- not listed: the rules already catch it. Tell the user, and offer to commit
  the sample alone so it stays caught.
- `MISSED spam <file>: closest rule <rule>, failed <condition> | ...`: the
  failed conditions say what the message lacked.
- `MISSED spam <file>: no rule that deletes is enabled`: say so; the fix is a
  rule, and it starts watch-only (below).

## 4. Find the smallest change

Try candidates smallest first, each on a scratch copy of the config (never the
real file), and keep the first that catches the new sample with zero legit hits:

1. Add one word or quoted phrase from the message to a word list the closest
   rule already uses (the failed `words: ...` condition names the lists).
2. Add it to another existing list that a rule pairs with a link or a contact
   request.
3. Only if no existing rule fits: a new rule built like the existing ones (a
   keyword plus a link or contact detail, or keywords from two lists), with
   `confirmed: false`, so it starts watch-only and reports before it acts.

Rules for every candidate:

- Never remove a condition from a rule, never loosen a `has:` list, never touch
  `mode`, `confirmed`, `never_match` or `allowed_domains` to catch spam.
- No single common word that ordinary members use; prefer a phrase.
- Words are quoted; a `*` only at the start or end of a word (docs/config.md
  "Writing words and rules").
- Test each candidate:
  `groupwarden corpus test --config <scratch copy> --corpus <corpus>`. It must
  show no `MISSED` line for the new sample and `legit_hits=0`; a candidate with
  any `FALSE-HIT` line is dropped.

For a false hit (step 2's legit case) the smallest change is usually a
`never_match` phrase or an `allowed_domains` entry for the innocent wording or
site, or removing the one word that caused it; it must keep `spam_missed=0`.

## 5. Show the user, apply only on yes

Show: the saved sample file, the diff to `config.yaml`, and the before/after
counts (`TOTAL` spam, legit, legit_hits, spam_missed, and each changed `RULE`
line). Say plainly when the word joins a list a confirmed rule uses: the bot
will start deleting, removing and banning on it as soon as the config syncs.
Apply the change to the real config only on the user's yes. On no, leave the
config alone and ask whether to keep the sample.

## 6. Check, commit, push

```bash
groupwarden check --config <config>
```

It must exit 0 (it also re-runs the corpus test against `corpus_dir`). Then in
the private config repo commit only the config and the corpus files this run
added (`git add <config> <files from ADDED/SEEDED>`, never `git add -A`), with a
message like `spam-intake: <list or rule> catches <what the spam was>`, and push.
Tell the user the commit and that the bot's sync timer loads it within
`config_sync_minutes`, posting `config v<hash> loaded` in the admin chat.

## Never

- Never write pasted samples, names or numbers into the public groupwarden repo
  or anywhere outside the private config repo. A public test sample is a
  separate, hand-written synthetic one.
- Never apply a change the user has not said yes to, or one that hits a legit
  sample.
- Never put a secret, token or real phone number in a commit message or note.
