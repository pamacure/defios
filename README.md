<div align="center">
  <img src="assets/3_2/DeFiOS_text_nobg-977x651.png" height="256"/>
</div>

A self-hosted multi-party wallet. Two machines each run one Node, form a Group on a headscale tailnet, and mint a 2-of-2 ECDSA Key and a 2-of-2 Ed25519 Key. Talk between them is carried on the COSL.

`serve` is the Node online: the Vault stays unlocked, the Node is on the tailnet, and Raft is running. Quit `serve` and the Node stops sending and receiving. The COSL replica stays on disk at the last applied row. Reads of that replica (`node inspect`, `node usage`, `group list`, `group inspect`, `session list`, `session inspect`, `key list`, `key inspect`, `conversation log`, `cosl log`, `cosl decode`) and `conversation prune` still work. Anything that appends, forms a Group, or runs a Session needs `serve`.

Run `serve` in one terminal and the other commands in a second terminal, as the same user. The local socket is mode `0600`.

Files land in `$XDG_CONFIG_HOME/defios` and `$XDG_DATA_HOME/defios` (`~/.config/defios` and `~/.local/share/defios` when those variables are unset). The socket is `$XDG_RUNTIME_DIR/defios/serve.sock`.

## Usage

Call the two machines A and B. Use a different `--name` on each. Leave `group new --hostname` unset; the default is `defios-`, the first 8 hex characters of the Node ID, and the Group alias (e.g. `defios-1a2b3c4d-personal`), so one device presents a distinct hostname on each Group's tailnet.

### 1. Headscale

Both machines join one stock headscale. MagicDNS is on, the embedded DERP relay is on, and the policy lets the two Nodes reach each other (an empty policy allows all). Both machines must reach the same `server_url`. This flow was exercised against headscale 0.29.5.

On the headscale host, create a user and one preauth key per machine:

```bash
headscale users create defios
headscale users list -o json
headscale preauthkeys create --user <id> --reusable --expiration 24h -o json
headscale preauthkeys create --user <id> --reusable --expiration 24h -o json
```

Give A the first key and B the second. When `headscale_url` is plain `http`, start both serves with `TS_DEBUG_USE_DERP_HTTP=1` so the embedded DERP region is dialed over HTTP.

### 2. Build

Go 1.27. On each machine, from a checkout:

```bash
CGO_ENABLED=0 go build -o defios ./cmd/defios
```

That binary is static. Copy it to the other machine when `GOARCH` matches, and put `defios` on `PATH`.

### 3. Init

`init` sets up the device only — identity and Vault — and generates ECDSA preparams before it returns. Stderr prints `generating ECDSA preparams`. That step is CPU-heavy and can take several minutes. Run it on both machines.

```bash
defios init --name laptop
```

The passphrase is prompted twice and must be non-empty. `DEFIOS_PASSPHRASE` or `--passphrase-file` skips the prompt. `serve` must unlock the Vault with that same passphrase. `--name` is the default display name Groups inherit.

Then scaffold a Group under a local alias and point it at the headscale control URL. This creates the Group's config and makes it the Current Group that `serve` and read verbs default to:

```bash
defios group new personal --headscale https://headscale.example
```

The alias (`personal` here) is local to this machine, must match `[a-z0-9][a-z0-9_-]{0,63}`, and never travels on the wire. A Node may hold any number of Groups, each on its own tailnet and with its own isolated state; `group new` focuses the one it creates. Switch the Current Group later with `defios group use <alias>` — it takes effect on the next `serve`, so a running `serve` keeps serving the Group it started on. `defios group list` shows every Group's alias, status, Group ID, and which is current (`*`).

Read verbs (`group inspect`, `node usage`, `cosl log`, `conversation log`, `key list`, `key inspect`) accept `--group <alias>` to read any Group off disk regardless of which is current or served — all non-secret state is plaintext, and a Key's `share=yes` comes from that Group's own encrypted shares over the passphrase.

```bash
defios node inspect
```

Record each Node ID. `group create` needs both.

`defios node usage` prints what this Node takes up on the machine: the binary, `identity` and the Vault, and each Group's COSL (with row and pruned-row counts), Raft, tsnet, Vault files and Workspace, with data, config and grand totals. While `serve` runs it adds serve's PID, uptime, CPU time, RSS, Go heap and goroutines; otherwise it says `serve not running`. It is a one-shot snapshot; use `top` or your OS's monitor for a live view.

### 4. Serve

On each machine, in the terminal that stays open:

```bash
export TS_AUTHKEY='<preauth key for this machine>'
defios serve
```

The Group's `config.toml` says `tailnet_auth_key = "$TS_AUTHKEY"`: a reference to a Secret, found in the environment or else in the Group's Vault. To keep the key out of the environment, run `defios secret set TS_AUTHKEY` (value on stdin) before the first `serve` instead. A config never holds a secret itself: RPC URLs under `[networks.<alias>]` and `api_key` under `[providers.<provider>]` are `"$NAME"` references too.

`serve` prompts for the passphrase once unless `DEFIOS_PASSPHRASE` or `--passphrase-file` is set. It serves the Current Group. One Node has one socket, so only one Group is live at a time: a second `serve` — for any Group — exits 2 with an "already running" message while the first keeps running. The first successful join stores that Group's tailnet state under its subtree; later serves reuse it.

From the other terminal on each machine:

```bash
defios node list
```

Each side prints the other Node's ID, display name, and hostname. Form the Group after that.

### 5. Group

On A, pass exactly two Node IDs, including A's own. A's create is A's accept.

```bash
defios group create <A-node-id> <B-node-id>
```

On B:

```bash
defios group accept <group-id>
```

B prints `Group <id> is valid`. Then on both:

```bash
defios group list
defios group inspect
```

Status is `valid`, the Group ID matches, and both Roster rows show `reachable=yes` and `joined=yes`. `joined` is that Node's `membership.join` on the COSL. If it is still `no`, wait and inspect again. A `not delivered` error means re-run the command once `node list` shows the other Node.

### 6. Keys

Mint ECDSA, then Ed25519. Only one Session may be open. `session start` is the initiator's local accept. The other Node's `serve` stderr prints `session <id> waiting for accept`. Accept there within 2 minutes. After that the Session aborts, and a new `session start` is a new Session ID.

On A:

```bash
defios session start --curve ecdsa --t 2
```

On B, using the printed Session ID:

```bash
defios session accept <session-id>
```

When the rounds finish, on both:

```bash
defios session list
defios session inspect <session-id>
```

Status is complete, with two completes that agree on curve and public key. Repeat with `--curve eddsa --t 2` only after the ECDSA Session is complete.

While `serve` is up:

```bash
defios key list
defios key inspect ecdsa:<hex-from-key-list>
defios key inspect eddsa:<hex-from-key-list>
```

Both machines show the same two `curve:hex` lines, `t=2 n=2`, and `share=yes`. `share=yes` is read from the unlocked Vault. Record each `conversation` line from `key inspect`.

### 7. Conversation

Group-level, from either machine:

```bash
defios conversation send "hello group"
defios conversation log
```

Both machines show the same lines. The human line is `<seq> <time> <display name> <writer>  <text>`, where the writer is `user` for a person's message and `@handle` for an agent's. `--json` gives each row its `handle` (`""` for a person) and `depth` (0 for a person).

After both Keys exist:

```bash
defios conversation send --key ecdsa:<hex> "about the ecdsa key"
defios conversation send --key eddsa:<hex> "about the eddsa key"
defios conversation log
defios conversation log --key ecdsa:<hex>
defios conversation log --key <ecdsa-conversation-id>
```

`--key` accepts `curve:hex`, a unique hex prefix of the public key, or the full Conversation ID hex. `conversation log` with no `--key` shows Group-level text. `conversation log --key` shows that Key's text. The same filter matches on both machines.

A Recap stands in for a stretch of talk. You supply the text; nothing in the Node writes it:

```bash
defios conversation recap --key ecdsa:<hex> --base-seq <seq-of-a-message> --text "what was said so far"
defios conversation log --key ecdsa:<hex>
```

`--base-seq` must be the seq of a message in that same Conversation, past the base of any earlier Recap. Without `--key` it recaps Group-level talk. `conversation log` then prints the Recap first, as `[recap of <n> messages to seq <base>] <text>`, followed only by the messages after its base. `cosl log` still shows every row.

```bash
defios conversation log --key ecdsa:<hex> --full
defios conversation log --key ecdsa:<hex> --lean
```

`--full` also shows the messages and earlier Recaps that the live Recap supersedes. A message whose payload this Node has pruned shows as `[pruned]` with only its seq, time and sender. `--lean` gives an agent its starting context: one `<display name>: <text>` line per entry (`<display name>/@handle: <text>` for an agent's), the live Recap first, then every message after it. Nothing else is printed, including pruned messages. Both flags work with `--key` and `--limit`. Under `--full`, `--limit` keeps the latest N rows. Otherwise it keeps the latest N messages after the Recap.

```bash
defios conversation prune --key ecdsa:<hex>
```

`prune` destroys the text of the messages the live Recap covers, on this machine only. Every row, its hash and its place in the chain stay, so the replica still verifies and still replicates. Nothing is appended and the other Node is not told, so the two machines can hold different amounts of talk. Recaps are never pruned. With no Recap it exits 1 and points at `conversation recap`. Running it again prunes nothing new. It does not need `serve`. A later snapshot restore from a Node that kept the messages brings them back, and `prune` prints a warning saying so unless this machine's prune policy is on.

To keep a machine light without running `prune`, set `prune = true` at the top of its Group `config.toml`. `serve` then prunes on startup, whenever a Recap commits and after every snapshot restore, in every Conversation, and logs how many messages it dropped up to which seq. It is off by default, and with it off `serve` never destroys a message. A running `serve` notices a change to it within about a minute, so turning it on or off needs no restart. If the file does not parse, `serve` logs that and keeps the setting it last read. One machine can prune while the other keeps everything: they still agree on the chain and replicate to each other.

```bash
defios cosl log
defios cosl decode <msg-id-of-a-key-scoped-row>
```

That row's `conversation_id` equals the `conversation` line from `key inspect` for that Key. `cosl log` for the whole run includes `membership.join`, `session.start`, `tss.session`, `session.complete`, and `app.freeform`.

An external runner can post an agent's reply. The Handle must be defined in this Group's `config.toml` and included in that Conversation's `[agent] profiles`. Otherwise the send exits 1 and nothing is appended:

```bash
defios conversation send --key ecdsa:<hex> --handle @auditor "looks fine"
defios conversation send --key ecdsa:<hex> --handle @auditor --depth 2 "and so does this"
```

`--depth` defaults to 1, works only with `--handle`, and must be between 1 and `max_depth`. Leaving out `--handle` means the message is a person's. `serve` checks the same rules for any client of its socket.

A person's message that mentions an agent (`@` followed by a Handle, with no letter, digit or `@` right before the `@` and no letter or digit right after the Handle, so neither `a@b.com` nor `@auditorX` is a mention, while `@auditor--` mentions `auditor`) must name a Handle included where it is sent. Otherwise the send exits 1, names the Handle and lists the included ones, and nothing is appended:

```bash
defios conversation send --key ecdsa:<hex> "@nobody, take a look"
```

An unknown Key exits 1:

```bash
defios conversation log --key ff
defios conversation send --key ff "nope"
```

### 8. Sign and broadcast on Linea Sepolia

Send a native-token transfer on Linea Sepolia (chain ID 59141) signed by the 2-of-2 ECDSA Key. The Key is a plain Ethereum account, so the chain sees an ordinary transaction. Sign Requests carry a transaction, not an arbitrary message.

On A, register the Network. It replicates to B:

```bash
defios network add linea-sepolia --type ethereum --chain-id 59141 --rpc-url https://rpc.sepolia.linea.build
defios network list
```

On either machine, get the Key's address and fund it from a Linea Sepolia faucet with a little test ETH:

```bash
defios key inspect ecdsa:<hex-from-key-list>
```

The `ethereum_address` line is the account. Check its balance on https://sepolia.lineascan.build/ before going on. The transaction's `nonce` is the account's transaction count, `0` for a fresh account.

On A, write the transaction. `to` is a base64 20-byte address; this converts a hex one (here, send to yourself or any address you control):

```bash
python3 -c "import base64,sys; print(base64.b64encode(bytes.fromhex(sys.argv[1].removeprefix('0x'))).decode())" 0x<recipient-address>
```

Without Python, GNU coreutils works (the hex must be uppercase), as does `xxd`:

```bash
echo <recipient-hex-without-0x> | tr a-f A-F | basenc --base16 -d | base64
echo <recipient-hex-without-0x> | xxd -r -p | base64
```

```bash
cat > tx.json <<'EOF2'
[{"network": "linea-sepolia",
  "ethereum": {"to": "<base64-from-above>", "value": "1000000000000", "nonce": "0",
               "gasLimit": "21000", "maxFeePerGas": "1000000000", "maxPriorityFeePerGas": "100000000"}}]
EOF2
```

`value` is in wei (here 0.000001 ETH). Propose it, which also records A's acceptance. The request stays open for 5 minutes unless you pass `--deadline <seconds>`:

```bash
defios sign propose --key ecdsa:<hex> --reason "linea sepolia test" --transactions tx.json
```

On both machines:

```bash
defios sign list
defios sign inspect <msg-id>
```

Status is `open` with `1/2 accepted`. `inspect` decodes the transaction (network, recipient, value, fee) and shows the digest every Node derived independently. Check that both machines show the same details and digest before accepting. Then on B:

```bash
defios sign accept <msg-id>
```

That is t=2, so the status becomes `waiting`, then `signing` once a signer starts the Session, then `signed` when a signature verifies against the Key. `waiting` can last a while if another Session (such as keygen) is still open. A request left `waiting` past its deadline becomes `expired`. A Session that aborts, for example by `sign decline` or a timeout, makes the request `aborted`; propose a new one to retry. A Node that accepts after two others did gets a `signer-set-full` error, which is not a failure of the request.

Once `sign list` shows `signed`, broadcast from either machine:

```bash
defios sign broadcast <msg-id>
```

It prints the transaction hash and records a receipt on the COSL. Open `https://sepolia.lineascan.build/tx/<hash>` to confirm it was mined, and `sign list` on both machines shows `broadcasts=1 (fully broadcast)`. Running `sign broadcast` again is harmless.

### 9. Limits

Signing covers Ethereum, Bitcoin and Solana transactions, not arbitrary messages. There is no fee estimation or nonce lookup: set `nonce`, `maxFeePerGas` and the rest yourself.

A 2-Node Group appends only while both Nodes are reachable. With one `serve` stopped, the other cannot commit. The stopped Node's replica stays at the last applied row and resumes when `serve` starts again.

Because only one Group is live at a time, a Group you switch focus away from is offline to its peers until you refocus it: it cannot append, keygen, or receive Conversation messages meanwhile. Refocusing (`group use <other>` then `serve`) reconverges it through Raft catch-up automatically, with no manual resync.
