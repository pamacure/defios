# A mention is found in the text alone, then resolved against config

A **mention** is `@` followed by a Handle, written in talk to summon that
agent. Finding a message's mentions uses only its text: no config, no list of
the Handles that exist. Resolving them, against the Handles included where
the message is sent, is a separate step. So any client (the serve, the CLI, a
later TUI or GUI, an external runner) finds the same mentions in the same
text, and can highlight or autocomplete them without knowing any Node's
config. The rule below, and the test vectors in
[`docs/mentions/vectors.json`](../mentions/vectors.json), are the contract for
those clients. This amends [ADR 0015](0015-agent-profiles-handles-and-agent-chains.md),
whose Mentions bullet gave the first version of the rule, and tightens its
Handle rule. No config with a Handle the new rule refuses had shipped, so
nothing is migrated.

## The rule

**Characters.** A *word character* is any code point in the Unicode general
categories L (letter), M (mark) or N (number): `[\p{L}\p{M}\p{N}]` with the
`u` flag in JavaScript, `unicode.IsLetter || unicode.IsMark ||
unicode.IsNumber` in Go. *ASCII alphanumeric* is `[A-Za-z0-9]`.

**A Handle** is one or more runs of `[a-z0-9]`, joined by single `-` or `_`,
1 to 64 characters, and not `user`:

```
handle = [a-z0-9]+ ( [-_] [a-z0-9]+ )*
```

So `audit`, `au-di` and `au_di_2` are Handles; `-au`, `au-`, `au--di` and
`Audit` are not. `user` is reserved for a person.

**Finding mentions.** Scan the text from the start. At each `@`:

1. If the character before it is a word character, `@` or `\`, it starts no
   mention. The start of the text counts as no character.
2. Otherwise take the longest text right after the `@` that matches
   `[A-Za-z0-9]+ ( [-_] [A-Za-z0-9]+ )*`. If nothing matches, there is no
   mention.
3. If the character right after that text is a word character, there is no
   mention.
4. Otherwise it is a mention of that text in ASCII lowercase.
5. Scanning resumes after the matched text.

A message's mentions are the Handles so found, in the order each first
appears, each once.

**What follows from it.**

- **No prefix matching.** `@audit` mentions `audit`, never `aud` or `audi`.
  `@auditing` mentions `auditing`, whatever Handles exist.
- **Case.** `@Auditor` and `@AUDITOR` mention `auditor`: a phone keyboard
  capitalises the start of a sentence. Handles themselves are lowercase.
- **Words around it.** `a@b.com` is not a mention (rule 1). `@auditorX` is
  a mention of `auditorx`, the whole word, not of `auditor`. `@auditoré` and
  `@auditor²` are not mentions (rule 3): the word goes on past what a Handle
  can spell.
- **Punctuation.** `@auditor,` `@auditor.` `(@auditor)` and `@auditor's`
  mention `auditor`. A `-` or `_` not followed by an ASCII alphanumeric is
  punctuation, so `@auditor-`, `@auditor--like` and `@auditor_` mention
  `auditor`. A single separator between alphanumerics is part of the Handle:
  `@auditor-like` mentions `auditor-like`.
- **Escape.** `\@auditor` is not a mention (rule 1). The stored text keeps
  the backslash; a client may display `\@` as `@`. There is no escape for the
  backslash itself: to show a backslash before a mention, put a space between
  them.
- **Too long, or reserved.** A match longer than 64 characters, or `user`,
  is still a mention, of a Handle that can never be included. A person's
  message with one is refused like any other mention that does not resolve.

**Resolving mentions.** Per ADR 0015: `conversation send` refuses a
person's message with a mention that names no Handle included where it is
sent, and nothing is appended. A mention in an agent's reply that does not
resolve is skipped.

## Considered options

- **Match mentions against the Handles that exist** (longest defined Handle
  that prefixes the text). Rejected: what a message mentions would depend on
  the config of whoever reads it, a client would need that config to
  highlight a mention, and adding `audit` beside `aud` would change what old
  messages mean.
- **Case-sensitive mentions.** Rejected: `@Auditor` at the start of a
  sentence would silently mention nobody, which is the worst outcome for a
  person who meant to summon it.
- **A Handle ending at any `-` or `_`, or allowing `--` inside one.**
  Rejected: `--` between words is ordinary prose, and a Handle ending in a
  separator could not be told from one followed by punctuation.
- **Mentions inside backticks don't count.** Rejected: talk is
  `text/plain`, and a markdown-shaped rule invites the rest of markdown's
  rules. One escape character is enough.
- **No escape** (write `@ auditor`). Rejected: a person quoting a Handle
  would have to misspell it.
- **Ignoring a too-long or reserved match.** Rejected: the person meant a
  mention, and refusing it tells them.

## Consequences

- `internal/mention` implements the rule, and its tests run the vectors
  file. A client in another language should run the same file.
- `config` validates Handles with the same grammar, so every Handle a config
  defines can be mentioned.
- A change to the rule changes this ADR and the vectors file together.
