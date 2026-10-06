---
domain: whatsapp-community-moderation
type: decision
topics: [whatsapp, communities, moderation, whatsmeow, uk-gdpr, automated-decisions, spam-rules]
use_when: "Changing how groupwarden talks to WhatsApp, its rule engine, its deployment, or its privacy templates"
github_issue: 1
created_date: 2026-10-05
last_reviewed: 2026-10-06
last_updated: 2026-10-06
status: active
sources:
  - url: https://developers.facebook.com/documentation/business-messaging/whatsapp/groups
    type: official
    accessed_date: 2026-10-05
  - url: https://www.whatsapp.com/legal/terms-of-service-uk
    type: official
    accessed_date: 2026-10-05
  - url: https://www.meta.com/en-gb/brand/resources/whatsapp/whatsapp-brand/
    type: official
    accessed_date: 2026-10-05
  - url: https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/online-safety-and-data-protection/content-moderation-and-data-protection/
    type: official
    accessed_date: 2026-10-05
  - url: https://github.com/tulir/whatsmeow
    type: community
    accessed_date: 2026-10-05
  - url: https://github.com/umputun/tg-spam
    type: community
    accessed_date: 2026-10-05
coverage: intermediate
related_code:
  - file: internal/client/whatsmeow
    description: "The only whatsmeow importer"
  - file: internal/rules
    description: "Combination rules, normalisation"
dependencies:
  - name: go.mau.fi/whatsmeow
    version: pinned pseudo-version in go.mod
    status: active
retention_policy: event-based
archive_trigger: "WhatsApp opens an official API for group moderation, or groupwarden changes client library"
---
# Research behind groupwarden (summary)

A public summary of the platform, library and legal research done before groupwarden was built (October 2026). Findings are as of then; WhatsApp, the libraries and the law all change. Figures marked *measured* came from runs during the research; the rest are quoted from the sources at the end.

## AI task lookup

| If you are… | Read | Why |
|---|---|---|
| Wondering why not the official API | [Platform](#platform) | It cannot delete or remove in groups it did not create |
| Changing the client library | [Libraries](#libraries) | What each library can and cannot do |
| Touching delete / remove / ban | [Platform](#platform), [Operations](#operations) | Deletes are never confirmed; bans follow sending too much |
| Writing or changing rules | [Rules](#rules) | Why keyword-only rules are not allowed to ban |
| Deploying or writing the privacy notice | [Legal](#legal) | DPIA, legitimate interests, Article 22C |

## Platform

| Finding | Source |
|---|---|
| The WhatsApp Business Platform's Groups API cannot do this job: groups of at most 8 participants, the business number only in groups it creates, and "Delete message" listed as not supported. Communities are not supported | Groups API documentation |
| A group admin can delete another member's message for everyone within two days of it being sent, and WhatsApp does not tell you if a delete for everyone failed | WhatsApp Help Center |
| Removing a member from a community removes them from every group of it; only the community's creator and admins can do that. Deleting a message needs admin in that group | WhatsApp Help Center (FAQ 632037368402886) |
| WhatsApp has no ban list for groups, and whether a removed person can rejoin through a still-valid invite link is not documented; so a moderator keeps its own list and removes a banned person who comes back | WhatsApp Help Center |
| Members can add new groups to a community by default; an admin setting limits it to admins | WhatsApp Help Center (FAQ 813798063285846) |
| Joining a group never makes you an admin, and nobody can promote themselves | WhatsApp Help Center |
| View-once messages are never delivered to linked devices, so no linked-device bot can read them | whatsmeow source (`UnavailableTypeViewOnce`) |

So a moderator has to be an **unofficial linked device** on its own number, made admin by a human in every group, keeping its own ban list and removing banned people who come back.

## Libraries

| Library | Verdict | Why |
|---|---|---|
| whatsmeow (Go, MPL-2.0) | **Chosen** | Admin delete of others' messages, per-person remove results, join by link, join-request approve/reject, community groups, typed disconnect events (logged out, replaced, client outdated, temporary ban), reconnects by itself, SQLite session store, and it decrypts replies to announcements and edits sent with message secrets. Gaps: join requests must be polled, no tagged releases (pin a commit), effectively one maintainer, and joining a community's groups without a link needs an internal call |
| Baileys (TypeScript) | Not chosen | Release candidate only; community metadata call broken; acknowledges a message before the app has it (a crash can lose one); cannot decrypt announcement replies or secret-encrypted edits |
| whatsapp-web.js | Rejected | Drives a headless browser (*measured* ~544 MB); "delete for everyone" can silently become delete for me; remove always reports success |
| neonize (Python, wraps whatsmeow) | Rejected | A message-decoding bug reproduced; no join-request approve/reject |
| whatsapp-rust | Alternative | MIT throughout (its own Signal code), can join community groups directly; young, one main author |

Both whatsmeow and Baileys depend on GPL-3.0 Signal libraries, so any binary or image built with them is GPL-3.0 (the source can still be MIT; see `NOTICE`).

## Operations

- A linked device needs a real phone with a real SIM; WhatsApp logs out linked devices when the phone has not used WhatsApp for 14 days.
- One session, one running copy: a second copy takes the session over ("stream replaced").
- Alerts must leave by another channel (Telegram here), because WhatsApp is what fails.
- After more than two days of downtime, older spam can no longer be deleted.
- whatsmeow's temporary-ban reasons are all about sending, and a delete is a sent message: rate-limit actions. Bot numbers in groups have been banned in waves.
- Keep at least two human admins in every group and a human community owner, so a banned bot number does not leave the community without admins.

## Rules

- On a small synthetic set (12 spam, 17 normal posts), keyword-only rules caught 6 spam and wrongly hit 9 normal posts; "finance word AND link" caught 5 and hit 3; "finance word AND lure AND contact detail" caught all 12 and hit none (*measured*, illustrative only). Hence: a keyword alone never removes anyone; acting rules need a keyword plus a link or contact detail, or words from two lists.
- A never-match list is needed ("in stock", "stock photo", "chicken stock").
- Spam hides in captions, invite links, polls, contact cards, event links and display names; quoted text belongs to someone else and must not count against the person replying; edits must be checked again.
- Normalise for matching only: NFKC, Unicode UTS #39 confusable skeletons, invisible characters, spaced-out letters.
- No user-supplied regular expressions (catastrophic backtracking in many engines; Go's RE2 is linear but rules stay word lists anyway).
- YAML 1.1 surprises (`NO` → false, `1.10` → 1.1, duplicate keys) need a strict loader that reports line numbers and keeps the last good config.
- Prior art: the Telegram bot tg-spam is the best design reference (watch-only mode, admin chat with unban); its issue #365, where a links-only rule banned long-standing members, is why link-alone never bans here. No maintained public WhatsApp moderator handled communities or re-joins.
- Account takeover is common, so trusted members are not exempt; the contest route and [Undo] cover the person whose account was hijacked.

## Legal

- **Terms of Service.** WhatsApp's Terms forbid accessing the service through automated or unauthorised means; the practical risk is the bot's number being banned. The Terms also say a banned user must not create another account without permission. No legal action against these open-source libraries was found.
- **Name and logo.** Meta's brand rules forbid WhatsApp marks in a product or account name; describe what the tool works with and say it is not affiliated.
- **UK GDPR.** The deployer is the controller. The ICO's content-moderation guidance says moderation is likely high risk, so a DPIA comes first; the lawful basis is legitimate interests with a written assessment; members must be told what is banned and how it is enforced.
- **Automated decisions.** Removing someone from a community with no human involved is likely a significant decision based solely on automated processing. Since 5 February 2026, UK GDPR Articles 22A–22D require safeguards: telling the person, letting them make representations, human intervention and a way to contest. "Recognised legitimate interest" (Article 6(1)(ea)) cannot be the basis for such decisions (Article 22B(4)).
- **Minimisation.** Decide on content, not on who the person is; keep copies only of posts acted on; set retention (groupwarden's defaults: evidence 30 days, action log 12 months, bans until lifted).
- **Children.** If any members may be under 18, the ICO expects extra care and its Children's code may apply.

The templates in `docs/privacy/` turn these points into a DPIA, a legitimate-interests assessment, a member notice and a record of processing.

## Sources

- WhatsApp Business Platform, Groups API: <https://developers.facebook.com/documentation/business-messaging/whatsapp/groups>
- WhatsApp Terms of Service (UK): <https://www.whatsapp.com/legal/terms-of-service-uk>
- WhatsApp Help Center articles on deleting messages, communities and group admins: <https://faq.whatsapp.com/> (articles 1370476507114859, 632037368402886, 813798063285846, 633713745095781, 441425088132574, 967457667545238, 742237357891080)
- Meta brand resources for WhatsApp: <https://www.meta.com/en-gb/brand/resources/whatsapp/whatsapp-brand/>
- ICO, content moderation and data protection (incl. automated decision-making and DPIAs): <https://ico.org.uk/for-organisations/uk-gdpr-guidance-and-resources/online-safety-and-data-protection/content-moderation-and-data-protection/>
- UK GDPR Articles 6, 22A–22D, 30 and 35: <https://www.legislation.gov.uk/eur/2016/679/contents>
- whatsmeow: <https://github.com/tulir/whatsmeow> (send.go revoke for group admins, msgsecret.go, types/events)
- Baileys: <https://github.com/WhiskeySockets/Baileys>
- whatsapp-web.js: <https://github.com/pedroslopez/whatsapp-web.js>
- whatsapp-rust: <https://github.com/oxidezap/whatsapp-rust>
- libsignal-protocol-go (GPL-3.0): <https://github.com/tulir/libsignal-protocol-go>
- tg-spam: <https://github.com/umputun/tg-spam> (issue #365)
- GNU GPL FAQ: <https://www.gnu.org/licenses/gpl-faq.en.html>
