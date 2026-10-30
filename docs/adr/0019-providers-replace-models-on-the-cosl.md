# Providers replace Models on the COSL

A Model config tied three things together: who is called, which API shape
is spoken, and which model answers. A fixed `provider` (`anthropic` or
`openai`) chose both the credential and the wire format, so OpenAI's
Responses API had no place to go except a third provider, and the same
OpenAI key would have had to be configured twice. We now replicate a
**Provider** instead: `{alias, base_url, api_path}`, and nothing more. The
alias is the Provider; there is no provider name behind it. The model, and
every other request option, moves into the Agent Profile's request body,
which the Node owning that profile writes in its own local config. DeFiOS
writes only what running an agent needs, and assumes nothing else about
the body. This replaces the Model registry of
[ADR 0012](0012-recap-on-the-log-pruning-off-it.md), and the endpoint rule
for credentials of
[ADR 0013](0013-group-config-workspaces-and-named-secrets.md). It amends the
profile keys of [ADR 0015](0015-agent-profiles-handles-and-agent-chains.md)
and [ADR 0018](0018-an-agent-says-one-message.md).

## Considered options

- **An explicit `api` field on the Model** (`messages | chat_completions |
  responses`), with provider and API shape kept apart. Rejected: more fields
  than the case needs. We have no gateways with custom routes to support yet,
  so the path's suffix is enough to tell the shape apart.
- **A new provider per API shape** (`openai-responses`). Rejected: it puts a
  wire format in the credential's name. As it turns out, one OpenAI endpoint
  with two shapes does now need two Providers (`openai` and, say,
  `openai-cc`), but those are aliases a Group picks, and both name the same
  Secret.
- **Keep the model on the log.** Rejected: one Provider serves many models.
  Which model an agent runs on is a choice about that agent, local to the
  Node that runs it, like its system prompt.
- **DeFiOS sets defaults per shape** (`stream`, a max-token bound, `store`).
  Rejected: each default is an assumption about a body DeFiOS does not own,
  and field names differ per shape. The profile's body says what is sent.
  The default agents we ship will set `stream = true`.
- **Refuse body keys that would break the agent loop** (`messages`, `input`,
  `tools`). Rejected: which keys those are depends on the shape, and an
  override that replaces the conversation is the operator's own mistake.
- **Keep ADR 0013's rule that a key goes only to a `base_url` this Node
  named.** Rejected: every member of a Group is trusted, and a `base_url`
  that lives on the log is the only endpoint a Provider has.
- **Accept `/v1/decisions` as a shape.** Rejected for now: it returns
  scores, not text, and takes no tools, so it cannot run an agent. Using it
  to judge something (a Sign Request's reason, say) is its own feature.

## Consequences

- **Provider registry.** `provider.create`, `provider.update` and
  `provider.remove` replace `model.*`, folded by alias as before. Both
  `base_url` (http or https, with no userinfo, query or fragment) and
  `api_path` (an absolute path, with no query or fragment) are required, and
  neither has a default. An update may change both; the alias cannot change.
  The call URL is `base_url` followed by `api_path`. No Node runs `model.*`
  rows, so they are dropped with no migration.
- **The path picks the shape.** The suffix of `api_path` decides the request
  shape, how the reply is read, and the auth headers:

  | Shape            | Path ends in        | Auth                                 |
  |------------------|---------------------|--------------------------------------|
  | Messages         | `/messages`         | `x-api-key`, `anthropic-version`     |
  | Chat Completions | `/chat/completions` | `Authorization: Bearer`              |
  | Responses        | `/responses`        | `Authorization: Bearer`              |

  Any other path is invalid. It is refused when written, and Fold skips it.
  More shapes, such as Gemini, are added as more rows.
- **Presets are CLI sugar.** `provider add <alias>` takes `--preset`; an
  alias that names a preset uses it unless told otherwise, and flags override
  a preset's fields. The row on the log is always complete, so Nodes on
  different builds read it the same way. The launch presets are `anthropic`
  (`https://api.anthropic.com` + `/v1/messages`) and `openai`
  (`https://api.openai.com` + `/v1/responses`).
- **Credentials.** `[providers.<alias>]` holds only `api_key`, a reference
  to a Secret as before, and it may be left out (for a local server, for
  example). The key goes to the Provider's `base_url` from the log. The
  local `base_url` override is gone.
- **Agent Profile.** `[agents.<handle>]` keys are `dir`, `provider` (a
  Provider alias, resolved at summon; an unknown alias fails the summon
  there) and `max_turns` (model calls per summon, default 8). The typed
  `model`, `max_tokens` and `temperature` keys are gone. A sub-table
  `[agents.<handle>.body]` holds the request's other fields, nested as TOML
  nests: `model` always, and, under the shape's own names, anything else,
  such as `stream`, `max_tokens` or `max_output_tokens`, `reasoning_effort`
  or `reasoning.effort`, and `store`.
- **The body.** DeFiOS writes only the conversation (system prompt and
  turns, in the shape's own fields) and the `say` tool. The profile's body
  merges over that: an object merges key by key, and any other value
  replaces what is there. Nothing is reserved, and no key can be removed. A
  body the API rejects is a 400 at summon time, and it is not retried.
- **Replies, streamed or not.** Each shape has a reader for an event stream
  and a reader for one JSON reply, chosen by the reply's `Content-Type`, not
  by the body's `stream`. Both send their blocks through the same stream
  events, so `conversation follow`, the web UI and Transcripts behave the
  same either way. A reply that was not streamed shows all at once when its
  call ends. A call that ends at its token bound is still an error, read
  from the stop reason the API returns: Messages' `max_tokens`, Chat
  Completions' `length`, or a Responses reply that comes back `incomplete`
  for `max_output_tokens`.
- **Responses.** Each call resends the conversation as input items. The
  reply's reasoning items are kept like Anthropic's thinking blocks and are
  replayed on the run's next call, so reasoning carries across a refused
  `say`. Whether OpenAI stores the response is the body's `store`.
  `previous_response_id` is not used.
