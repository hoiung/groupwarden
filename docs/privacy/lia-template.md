# Legitimate interests assessment (LIA) — template

The written three-part test for relying on legitimate interests (UK GDPR Article 6(1)(f)) to run groupwarden. Complete it with the DPIA (`dpia-template.md`), one per controller. Replace everything in [brackets]. It is a template, not legal advice.

## 1. Purpose test: is there a legitimate interest?

- **The interest:** protecting the members of [community] from scams and spam (crypto and investment pitches, fake job offers, phishing links) and keeping the groups usable for [what the community is for].
- **Who benefits:** the members, who are the targets of the scams; the admins, who would otherwise remove spam by hand.
- **Evidence of the problem:** [how much spam, how often, any member who lost money or left because of it].
- **Is it lawful and ethical?** Moderating a community you run, under rules the members are told about, is an ordinary interest. [Anything specific to your community.]

## 2. Necessity test: is the processing necessary for it?

- **Could you do it with less?** WhatsApp's own tools (approving new members, resetting the invite link) cannot tell a spammer from a real person at join time, and admins cannot watch every group at every hour; spam is often posted at night. [Your experience.]
- **Minimisation in the design:** it decides on the content of the post, not on who the person is; it keeps a copy only of posts it acts on; message text is only logged at debug level and phone numbers and IDs are masked in logs; it reads only the groups it is configured for.
- **Is the bot's response proportionate?** [Your reasons for delete + remove + ban everywhere with no warning, e.g. the accounts posting these are almost always fake or hijacked, and a warning gives a scammer more time in the group.]

## 3. Balancing test: do members' interests override yours?

- **Reasonable expectations:** members are told in every group before it starts (`member-notice-template.md`). [Is moderation already in your group rules?]
- **Impact on a member wrongly removed:** loss of access to the community until an admin restores it. Mitigations: combination rules only, watch-only start for new rules, the corpus test, evidence reported to admins, one-step [Undo], a contest route in the notice.
- **Vulnerable members:** [are any members under 18 or otherwise vulnerable? what extra care?]
- **Data shown outside the node:** the deleted message and attachment are shown to admins in a private Telegram group (text for [30 days], attachments for [24 hours]).
- **Sharing across communities:** [if the ban list is shared with another community, why that is fair to members of each].

## Outcome

[Legitimate interests applies / does not apply], because [reasons]. Decided by [name, role] on [date]. Review by [date, or when the rules, communities or retention change].
