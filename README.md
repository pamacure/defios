<div align="center">
  <img src="assets/3_2/DeFiOS_text_nobg-977x651.png" height="256"/>
</div>

A self-hosted multi-party wallet. Two machines each run one Node, form a Group on a headscale tailnet, mint threshold Keys together, and talk (with each other and with agents) over the COSL, a replicated log.

## Installation

You need Go 1.27 and a [headscale](https://github.com/juanfont/headscale) server both machines can reach, with MagicDNS and the embedded DERP relay on.

On the headscale host, create a user and one preauth key per machine:

```bash
headscale users create defios
headscale users list -o json
headscale preauthkeys create --user <id> --reusable --expiration 24h -o json   # for machine A
headscale preauthkeys create --user <id> --reusable --expiration 24h -o json   # for machine B
```

## Building

Build a static binary on each machine (or build once and copy it over) and put it on `PATH`:

```bash
CGO_ENABLED=0 go build -o defios ./cmd/defios
```

## Usage

The steps below use two machines, A and B. Run `defios serve` in one terminal and every other command in a second terminal, as the same user.

### 1. Start two Nodes

On each machine, create the Node's identity and passphrase-protected Vault (this takes a few minutes):

```bash
defios init --name laptop      # use a different name on each machine
```

Create a local Group config named `personal` that points at your headscale server:

```bash
defios group new personal --headscale https://headscale.example
```

Store that machine's preauth key in the Group's Vault (paste it at the prompt):

```bash
defios secret set TS_AUTHKEY
```

Bring the Node online, and leave it running:

```bash
defios serve
```

Check it: `defios secret show TS_AUTHKEY` says `vault`, `defios node inspect` prints this Node's ID (note it down) and `defios node usage` shows what it stores on disk.

### 2. Connect them

From the second terminal on each machine, discover the other Node on the tailnet:

```bash
defios node list
```

Check it: each machine lists the other's Node ID, display name and hostname.

### 3. Create a Group

On A, propose a Group of both Nodes:

```bash
defios group create <A-node-id> <B-node-id>
```

On B, accept it using the Group ID A printed:

```bash
defios group accept <group-id>
```

Check it: `defios group list` and `defios group inspect` on both machines show the Group as `valid`, with both Nodes `reachable=yes` and `joined=yes`.

### 4. Create a Key

On A, start a 2-of-2 key generation Session:

```bash
defios session start --curve ecdsa --t 2
```

On B, accept it using the Session ID A printed:

```bash
defios session accept <session-id>
```

Check it: `defios session list` shows the Session complete, and `defios key list` and `defios key inspect ecdsa:<hex>` show the same Key on both machines with `share=yes`.

### 5. Send messages

Post a message to the whole Group:

```bash
defios conversation send "hello group"
```

Post a message in the Key's own Conversation:

```bash
defios conversation send --key ecdsa:<hex> "about this key"
```

Check it: `defios conversation log` (Group talk) and `defios conversation log --key ecdsa:<hex>` (the Key's talk) show the same messages on both machines, and `defios cosl log` shows every row on the log.

### 6. Add a Provider

A Provider is an endpoint an agent calls: a base URL and an API path. It replicates to the whole Group. The `anthropic` preset is Anthropic's Messages API:

```bash
defios provider add anthropic
```

The `openai` preset is OpenAI's Responses API:

```bash
defios provider add openai
```

For any other endpoint, give both fields. The path's suffix picks the API shape, `/messages`, `/chat/completions` or `/responses`:

```bash
defios provider add local --base-url http://127.0.0.1:8080 --api-path /v1/chat/completions
```

To reach the same endpoint in another shape, add a second Provider. For example, OpenAI's Chat Completions next to the `openai` preset; both can use the same key:

```bash
defios provider add openai-cc --preset openai --api-path /v1/chat/completions
```

Store the Provider's API key in this Node's Vault (it never replicates):

```bash
defios secret set ANTHROPIC_API_KEY
```

Check it: `defios provider list` shows the Provider on both machines, and `defios secret show ANTHROPIC_API_KEY` says where the key comes from.

### 7. Create an agent

Write the agent's system prompt into its profile directory:

```bash
mkdir -p ~/.config/defios/agents/auditor
echo "You review proposed transactions and point out anything risky." \
  > ~/.config/defios/agents/auditor/SYSTEM.md
```

Then edit `~/.config/defios/group/personal/config.toml` to include the agent in Group talk under the existing `[agent]` table, bind it to the Handle `@auditor` on the `anthropic` Provider, and give that Provider the API key:

```toml
[agent]
profiles = ["auditor"]

[agents.auditor]
provider = "anthropic"

[agents.auditor.body]
model = "claude-opus-5-5"
stream = true
max_tokens = 8192

[providers.anthropic]
api_key = "$ANTHROPIC_API_KEY"
```

The `body` table is the rest of the request, in the API's own field names: DeFiOS writes only the conversation and the `say` tool, and the body merges over them. Nested tables become nested JSON, so `[agents.auditor.body.thinking]` with `type = "adaptive"` sends `"thinking": {"type": "adaptive"}`. A local server with no key needs no `[providers.<alias>]` table.

An agent on the `openai` preset takes the Responses API's own fields:

```toml
[agents.auditor]
provider = "openai"

[agents.auditor.body]
model = "gpt-5"
stream = true
max_output_tokens = 8192
store = false
include = ["reasoning.encrypted_content"]

[agents.auditor.body.reasoning]
effort = "medium"

[providers.openai]
api_key = "$OPENAI_API_KEY"
```

Each call resends the whole conversation, and the model's reasoning from earlier calls of the same turn goes back with it. With `store = false`, OpenAI keeps nothing, so ask for `reasoning.encrypted_content` to carry that reasoning across calls.

Mention the agent to have it answer:

```bash
defios conversation send "@auditor, introduce yourself"
```

Check it: `send` prints `@auditor: ...` when the agent replies, and `defios conversation log` shows the reply on both machines.
