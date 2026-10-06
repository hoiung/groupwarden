# Data protection impact assessment (DPIA) — template

For deployers in the UK (UK GDPR and the Data Protection Act 2018). The ICO's content-moderation guidance says content moderation is likely to result in a high risk, so complete a DPIA **before** groupwarden processes any real member's data. One DPIA per controller: if you run it for two communities with different controllers, each completes its own (or you write one joint DPIA naming both). Replace everything in [brackets]. It is a template, not legal advice.

## 1. The processing

- **Controller(s):** [name, contact]. [If two communities share the bot: name each community's controller, and say who decides what.]
- **Purpose:** keep scam and spam posts (for example crypto and investment pitches) out of the groups of [community/communities].
- **Who is affected:** members of [N] groups ([about N] people). **Are any members under 18?** [Yes / No / Unknown — how you know.] If any may be, record the extra care you take (the ICO's Children's code may apply), or how you keep under-18s out.
- **Data:** every new group message (text, captions, links, contact cards, polls, event links), the sender's WhatsApp IDs (LID, and phone number where WhatsApp shows it) and display name; for a post judged spam, a copy of the full message and any attachment, the action record (group, rule, config version, time), and the ban list entry.
- **Where it runs:** [the node: a machine you control, and where it is]. Data stays on that machine and its backup disk, apart from the admin reports in Telegram (section 4).
- **Lawful basis:** legitimate interests (Article 6(1)(f)), with the written assessment in `lia-template.md`. Do not use "recognised legitimate interest" (Article 6(1)(ea)): Article 22B(4) does not allow solely automated significant decisions on that basis.

## 2. Automated decisions: Article 22A–22D

A post that matches a confirmed rule is deleted and its sender removed and banned with no human involved. Being removed and kept out of a community is likely a "significant decision" made solely by automated means. Article 22C requires safeguards; record how each is met:

| Article 22C safeguard | How groupwarden meets it | Your notes |
|---|---|---|
| Tell the person about the decision | The member notice (`member-notice-template.md`) posted in every group says what is scanned, what is done and how to contest. [Do you also message a removed person? groupwarden itself sends nothing on WhatsApp.] | |
| Let them make representations | The notice gives a contact route: [an admin / email / form]. | |
| Human intervention | Every action is reported to the admins' Telegram group with the evidence; an admin reviews it. | |
| Contest the decision | An admin reverses a ban in one step: **[Undo]** on the report, `/unban`, or re-adding the person by hand (the bot does not reverse a human re-add). | |

The rules are written by people, combine a keyword with a link or contact detail (or two word lists), and a new rule starts watch-only until an admin confirms it: record that here as the design that keeps wrong removals rare.

## 3. Shared across communities

[If one bot serves more than one community:] one Telegram admin chat and one ban list (`bans.scope: all_communities`) are shared across [community A] and [community B], which may have different controllers. Record: who the admins of the shared chat are and which community each acts for; that a ban in one community removes the person from the other; the arrangement between the controllers (joint controllers, or one processing for the other) and who answers members' requests. [If `per_community`: each community keeps its own ban list; say so.]

## 4. What admins see in Telegram

The full deleted message and any attachment are shown to the admins in a private Telegram group (operated by Telegram, outside the node). The message text stays in the chat for [30 days] (`retention.evidence_days`), then the bot removes it from its reports, with the sender's display name and the attachment's file name (`groupwarden member forget` does the same at once); an attachment is posted for [24 hours] (`report.attachment_show_hours`) and can be shown again while the evidence copy is kept. Record: who is in that group, that it is private, and that admins must not forward evidence elsewhere.

## 5. Retention

| Data | Kept for | Setting |
|---|---|---|
| Evidence copy of a deleted message and its attachment | [30 days] | `retention.evidence_days` |
| Action log | [12 months] | `retention.action_log_months` |
| Ban list | Until an admin lifts the ban | — |
| Announcement-group message secrets (so replies can be read) | [90 days] | `retention.announcement_secret_days` |
| Encrypted backups of the database | The last [14] nights, then they age out | `backup.keep` |

`groupwarden member show` prints everything held about one person (for an access request) and `groupwarden member forget` deletes it (an active ban is kept, as the record of why they are kept out).

## 6. Risks and measures

| Risk | Likelihood, severity | Measures |
|---|---|---|
| A real member is removed by a wrong match | [ ] | Combination rules only; never-match phrases; corpus test of every rule against labelled spam and legitimate posts before it loads; new rules watch-only; [Undo] |
| A hijacked account is removed | [ ] | Reported with evidence; contest route in the notice; [Undo] once they have secured the account |
| Evidence leaks from the Telegram group | [ ] | Private group, admins only; text removed after [30 days]; attachments taken down after [24 hours] |
| The node or its backups are stolen | [ ] | Data on one machine you control; backups encrypted with age, the private key kept off the node |
| The bot's number is banned by WhatsApp (unofficial client) | [ ] | Human admins in every group keep the community running; see `docs/runbook.md` |
| Data kept too long | [ ] | Automatic purges (section 5) |

## 7. Sign-off

- Consulted: [admins, members' representatives if any]. DPO (if you have one): [name, advice].
- Residual risk: [acceptable / not — if high and not reducible, consult the ICO before starting].
- Approved by: [name, role, date]. Review by: [date, or when the rules or communities change].
