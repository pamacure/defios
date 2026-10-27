# One config.toml per Workspace; Secrets by name, in the Vault or the environment

A Node's local settings for a Group live in one `config.toml` at
`config/group/<alias>/`, the Group's **Workspace**, and each Key's
Conversation has its own Workspace at `config/group/<alias>/<conversation-id>/`
whose `config.toml` may override part of the Group's. A config never holds a
secret: a secret-bearing field holds a reference, `"$NAME"`, and the value is
found under that name in the serve's environment or else in the Group's Vault,
which stores **Secrets** by name and nothing about what they are for. This
amends [ADR 0008](0008-networks-on-cosl.md) (`networks.toml`) and
[ADR 0012](0012-recap-on-the-log-pruning-off-it.md) (credentials keyed by
Model alias, the `conversation/` path, `group-level`), none of which shipped
to a deployed Node, so nothing is migrated.

## Considered options

- **Credentials keyed by Model alias in the Vault** (#49 as first built) —
  rejected: the Vault knew which alias a secret was for, but not which life of
  that alias. After `model remove claude` and `model add claude --provider
  openai`, the Anthropic key was handed to OpenAI. A COSL message cannot
  clear another Node's Vault, so the binding has to be local and explicit.
- **Binding each stored credential to the Model's `model.create` row** —
  rejected: correct, but it puts COSL identity into the Vault and still leaves
  a Group-chosen `base_url` able to collect every Node's key. Naming the
  Secret in the Node's own config is simpler and covers Networks as well.
- **References keyed by Model alias** (`[models.claude] api_key = ...`) —
  rejected for the same reason: an alias can change provider across a remove
  and re-add. A reference keyed by provider is only ever sent to a Model of
  that provider.
- **Interpolating a Secret inside a string** (`"https://.../v2/${KEY}"`) —
  rejected: an RPC URL that embeds a key is itself the secret. A
  whole-value reference needs no escaping and no partial-string rules.
- **One Secrets file per Node** — rejected: ADR 0007 keeps a serve to its own
  Group's secrets, and a Node-wide file would unlock every Group's API keys to
  serve one. Environment variables are per serve, not per Node, and a serve
  serves one Group, so a per-Group Vault file has the same scope.
- **Conversation Workspaces created by the operator on demand** — rejected:
  the Workspace is where a Conversation's markdown (its `SOUL.md`) will live,
  and a Key whose agent has no Workspace to read is a gap, not a default.

## Consequences

- **Group `config.toml`**, at the Group Workspace root. Top-level keys are this
  Node's tailnet presence and Node policy, and may appear only here:
  `headscale_url`, `display_name`, `hostname`, `tailnet_auth_key`, and (#52) `prune`.
  Three sections may also appear in a Conversation Workspace's `config.toml`:
  - `[networks.<network-alias>]` `rpc_urls = ["$NAME", ...]`: this Node's
    local RPC endpoints, replacing `networks.toml` (ADR 0008). Every entry is
    a reference: an endpoint without a key belongs on the Network config on
    the COSL, where the whole Group shares it.
  - `[providers.<provider>]` `api_key = "$NAME"` and optionally `base_url`:
    the credential for every Model of that provider, and an endpoint this Node
    trusts with it.
  - `[agent]` `model = "<model-alias>"`, `token_window = <n>`: what the agent
    for Group-level talk, or for that Conversation, runs on. Nothing reads it
    yet.
- **Conversation `config.toml`** may hold only those three sections; any
  top-level key is an error. It merges over the Group's table by table: a
  Conversation that sets `[agent] model` keeps the Group's `token_window`, and
  one that sets `[networks.eth]` keeps the Group's `[networks.btc]`. A Sign
  Request is Key-scoped, so its broadcast uses its Key's merged config.
- **A credential goes only where this Node said.** The endpoint a Model is
  called at is the local `[providers.<p>] base_url` if set, else the Model's
  `base_url` from the COSL, else the provider's default. The provider's
  `api_key` is attached only to a local `base_url` or the provider's default,
  never to a `base_url` that exists only on the log, because any Node in the
  Group can set that one.
- **Secret-bearing fields** are exactly `tailnet_auth_key`, `rpc_urls` entries and
  `api_key`. Each must be a reference, `"$NAME"`, with `NAME` matching
  `[A-Z_][A-Z0-9_]*`. A literal is refused when the file is read, so a
  plaintext secret in a config is an error, not a convention. No other field
  is ever expanded, so a `$` elsewhere needs no escaping.
- **Resolution** of `$NAME`: the environment of the process reading the
  config (the serve, or a CLI verb) when the variable is set and non-empty,
  else the Group's Vault. A generic name already set for some other tool, such
  as `ANTHROPIC_API_KEY`, wins over the Vault silently; an operator who wants
  the Vault's value picks a name nothing else sets. A reference that resolves
  nowhere is an error where it is used, naming the reference and the Group.
- **`tailnet_auth_key`** follows the same rule. `group new` writes
  `tailnet_auth_key = "$TS_AUTHKEY"`, so `TS_AUTHKEY` in the environment works as
  before, and an operator may instead `secret set TS_AUTHKEY` before the
  first serve. The serve unlocks the Vault before it joins the tailnet, so a
  Vault-held preauth key is available on first boot. A Node already joined
  needs no preauth key, so here alone an unresolved reference is logged and
  the serve joins on its saved tailnet state rather than refusing to start. The field was `auth_key`
  and accepted a literal; it is renamed because the glossary's **Auth
  keypair** is the Ed25519 key that signs COSL messages, which lives only in
  the Vault's shared secrets and never in any config.
- **The Vault** gains one file per Group, `vault/secrets-<alias>`, holding
  `name -> value` (`VaultContents.secrets`, field 6, replacing the unshipped
  `model_credentials`). `defios secret set|show|remove <NAME> [--group]`
  replaces `model credential`: `set` reads the value from stdin, `show` says
  whether it comes from `env`, `vault` or `none`, and `--reveal` prints it.
  `init` and `group new` write no Secret.
- **Workspaces.** `group new` creates the Group Workspace. The serve creates a
  Conversation Workspace for every Key the log says is minted, as it starts
  and as Keys are minted, so every Node holds one. Markdown in either (the
  Group's or a Conversation's `SOUL.md`) is not parsed yet. A Conversation ID
  is 64 hex characters, so it cannot collide with a file in the Group
  Workspace. `group-level` and the `conversation/` directory are gone: the
  Group Workspace is where Group-level talk's agent is configured.
