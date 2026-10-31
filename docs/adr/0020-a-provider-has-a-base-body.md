# A Provider has a base body

Under [ADR 0019](0019-providers-replace-models-on-the-cosl.md) every request
option lives in each Agent Profile's body, so options that belong to the
Provider, not the agent, had to be repeated in every profile that runs on
it: `stream`, OpenAI's `store` and `include`, or a default model. A Node's
`[providers.<alias>]` table now takes a `body` sub-table as well as
`api_key`. It is the base request body for every profile on that Provider.

A request is built in three layers, each merged over the one before it by
ADR 0019's rule (an object merges key by key, and any other value
replaces what is there):

1. what DeFiOS writes: the conversation and the `say` tool;
2. the Provider's body, `[providers.<alias>.body]`;
3. the profile's body, `[agents.<handle>.body]`.

A profile with no body runs on its Provider's body alone. One that sets a
field overrides only that field. As before, nothing is reserved and no key
can be removed: a profile can set `stream = false` over a Provider's
`stream = true`, but cannot unset a key.

`[providers.<alias>]` is a Workspace section, so a Conversation's
config.toml may set its own Provider body too. It merges over the Group's
body for that Provider, the same way a profile's body merges over the
Provider's. The body is local config, like the key. It is not on the COSL:
the Provider row stays `{alias, base_url, api_path}`, and each Node decides
what its own agents send.

## Considered options

- **Keep every option in the profile.** Rejected: settings such as
  `stream` and `store` follow the Provider and repeat in every profile.
- **Put the base body on the COSL Provider row.** Rejected: ADR 0019 keeps
  the row down to who is called and how, so Nodes on different builds read
  it the same way. Request options are each Node's own choice, like its
  key.
- **Let a profile remove a Provider's key** (for example with a sentinel
  value). Rejected for now: TOML has no null, and nothing needs it yet.
  Overriding a key with another value covers `stream` and `store`.
