# DeFiOS MVP spec

This is the implementable spec for the DeFiOS MVP. Vocabulary is `GLOSSARY.md`. The *why* of each fork lives in `docs/adr/`; this file is the *what*. An implementing session is done when every acceptance criterion in [Acceptance](#acceptance) is green.

The MVP is a Go CLI per Node, on Linux and macOS, against a user-provided stock headscale. A 2-Node Group forms in-band on an already-up tailnet. Two sequential 2-of-2 keygen Sessions mint one ECDSA Key and one Ed25519 Key. The COSL carries every Key-related message and every Conversation. There is no signing function and no chain integration.

---

## 1. Architecture

One binary, two roles on the same machine:

| Role | Process | Holds secrets | Talks to |
|---|---|---|---|
| **CLI** | `defios <verb>` | no (except `init`, which writes the Vault) | `serve` over a unix socket, or plaintext files for reads |
| **`serve`** | long-running | unlocked Vault in memory | other Nodes over tsnet; local CLI over the socket |

`serve` is the only process that signs, unseals, runs Raft, or writes sqlite. Two `serve`s on the same data dir refuse (socket already bound).

### 1.1 Components

```
                    this machine                         tailnet
                 ┌──────────────────┐              ┌─────────────────┐
  operator ───►  │ CLI              │              │ other Nodes     │
                 │  (cobra verbs)   │              │  Raft / forward │
                 └────────┬─────────┘              │  formation HTTP │
                          │ unix socket            └────────▲────────┘
                          ▼                                 │ tsnet
                 ┌──────────────────┐                       │
                 │ serve            │◄──────────────────────┘
                 │  identity        │
                 │  Vault (unlocked)│
                 │  tsnet.Server    │
                 │  Raft + FSM      │
                 │  COSL sqlite     │
                 │  Session layer   │
                 │  KeygenEngine    │
                 └──────────────────┘
```

| Component | Owns | Does not own |
|---|---|---|
| **Identity / formation** | Node ID, signed Identity, Create proposal, accept/decline, Roster at start | COSL types, Raft log |
| **Vault** | auth seed, encryption private key, ECDSA preparams, Key shares | COSL replica |
| **COSL FSM** | envelope verify, sqlite insert, `prev_hash` | Session policy, engine |
| **Raft** | total order, commit, catch-up | signatures, Key policy |
| **Session layer** | start/abort/complete rules, local accept, engine lifecycle | Raft quorum |
| **KeygenEngine** | tss rounds, share bytes, joint public key | COSL, sealing, Node IDs as wire fields |
| **Conversation** | `app.freeform` filter by Conversation ID | a second store |

Recommended packages (implementers may regroup, not leak across the table above):

```
cmd/defios/
internal/identity/
internal/vault/
internal/cosl/          # envelope, registry, sqlite, FSM
internal/raftx/         # StreamLayer, bootstrap, forward gate
internal/group/         # formation HTTP, Roster
internal/session/       # orchestration, local accept
internal/engine/        # KeygenEngine interface
internal/engine/tssl/   # bnb-chain/tss-lib adapter
proto/
e2e/
```

### 1.2 One Group per Node

A Node belongs to at most one Group at MVP. A valid Group rejects further Create proposals. A pending proposal occupies that slot until it becomes valid or is declined.

### 1.3 Environment

| Item | Value |
|---|---|
| Module | `defios.com` |
| Go | 1.27, `CGO_ENABLED=0` |
| TSS engine (MVP) | `github.com/bnb-chain/tss-lib/v4` at `v4.0.0-20260923032227-f204718ae595` |
| Ordering | `github.com/hashicorp/raft` v1.8.x + `github.com/hashicorp/raft-boltdb/v2` |
| sqlite | `modernc.org/sqlite` |
| Vault | `filippo.io/age` scrypt |
| Sealing | `golang.org/x/crypto/nacl/box` |
| Transport | `tailscale.com/tsnet` against stock headscale (MagicDNS on, embedded DERP) |
| Envelope | `google.golang.org/protobuf` proto3 |

Post-MVP engine (not shipped): LFDT-Lockness `cggmp24` + `givre` via wazero. The KeygenEngine seam and the COSL `tss.session` wrapper are the swap point ([Verify the Lockness Rust stack as DeFiOS's TSS engine](https://github.com/pamacure/defios/issues/11)).

---

## 2. Identifiers and encodings

All 32-byte ids display as lowercase hex. SHA-256 is `crypto/sha256`. Domain strings are ASCII with no terminator.

### 2.1 Node ID

Computed once at `init` from the original auth public key, stored in `identity`, never recomputed:

```
Node ID = SHA-256( "defios-node-id-v1" || auth_public_key )
```

`auth_public_key` is the 32-byte RFC 8032 Ed25519 public key. Restore: `init --auth-key` with the same 32-byte seed reproduces the same Node ID. A later rotated auth key does not.

### 2.2 Group ID

```
Group ID = SHA-256( "defios-group-id-v1" || node_id_1 || node_id_2 || … )
```

Node IDs are sorted as raw 32-byte values (`bytes.Compare`) before concatenation. Same Roster ⇒ same Group ID.

### 2.3 Session ID

32 cryptographically random bytes, minted by the initiator. Identifies the run, not the Key.

### 2.4 Key identity and canonical public keys

A Key is identified by **curve + canonical public-key bytes**. CLI display form:

```
ecdsa:<33-byte-hex>
eddsa:<32-byte-hex>
```

Canonical export, used on the wire in `session.complete`, in Conversation ID derivation, and in the Vault share map:

| Curve | Wire name | Canonical public key |
|---|---|---|
| secp256k1 ECDSA | `ecdsa` | 33-byte SEC1 compressed (`0x02`/`0x03` ∥ X). From tss-lib `ECPoint`, use `btcec` compressed serialize. |
| Ed25519 | `eddsa` | 32-byte RFC 8032: little-endian Y with the sign bit of X in the high bit of the last byte. tss-lib has no encoder; the adapter writes this. |

`--key` accepts the full `curve:hex`, a unique hex prefix of that public key, or the Conversation ID hex. Ambiguous prefixes error.

### 2.5 Conversation ID

```
Conversation ID = SHA-256( "defios-conversation-id-v1" || curve || canonical_public_key )
```

`curve` is the ASCII wire name (`ecdsa` or `eddsa`). Nil / unset `conversation_id` on a COSL envelope is Group-level talk, not a Conversation.

### 2.6 Signatures

Every signed blob (COSL envelope, Identity, Create proposal, accept, decline) is Ed25519 over the protobuf's canonical serialization with the `sig` field unset (empty). Canonical means `proto.MarshalOptions{Deterministic: true}` on a schema that contains **no maps**. Signature is 64 bytes. Verify against the auth public key pinned for that Node ID.

### 2.7 Sealed boxes

Point-to-point payloads use NaCl `box` (X25519 + XSalsa20-Poly1305):

```
ephemeral X25519 keypair (per box)
nonce = 24 random bytes
ciphertext = box.Seal(plain, nonce, recipient_enc_pub, ephemeral_priv)
```

The ephemeral public key and nonce ride next to the ciphertext. Broadcast payloads are plaintext. A Node's share of a Key never leaves that Node in readable form.

---

## 3. Node storage

Decided in [Design Node storage: encrypted keyshare container and COSL sqlite schema](https://github.com/pamacure/defios/issues/8); why in [ADR 0005](../adr/0005-node-storage-vault-and-cosl-sqlite.md).

### 3.1 Directory lookup

Every command, including `init`:

1. `--dir` — config + data in one tree (tests)
2. else `DEFIOS_CONFIG_DIR` / `DEFIOS_DATA_DIR` if set
3. else `$XDG_CONFIG_HOME/defios` and `$XDG_DATA_HOME/defios` (`~/.config` / `~/.local/share` when those XDG vars are unset)

Runtime socket: `$XDG_RUNTIME_DIR/defios/serve.sock`; on macOS, if `XDG_RUNTIME_DIR` is unset, `$XDG_DATA_HOME/defios/serve.sock`.

All files `0600`, directories `0700`.

```
$XDG_CONFIG_HOME/defios/
  config.toml                 # headscale URL, display name, tsnet hostname

$XDG_DATA_HOME/defios/
  identity                    # JSON; Node ID written once, current auth/enc public keys
  vault                       # binary age(scrypt); private keys and shares
  tsnet/
  group/<group_id_hex>/
    roster.json               # signed Create proposal + Accepts held (§6.2); pending or valid; keep this — without it a Node cannot resume its Group and must form again (§4.4)
    cosl.sqlite
    raft.db                   # raft-boltdb LogStore + StableStore
    raft-snap/                # FileSnapshotStore

$XDG_RUNTIME_DIR/defios/serve.sock
```

Backup of this Node is `identity` + `vault`. `group/` is disposable if a majority still has the replica, except `roster.json`: it is the Node's record of its Group, and without it the Node has no Group to resume and must form again (§4.4). A pending proposal that is lost is simply proposed again. Lost Vault = new Node.

### 3.2 `identity` (JSON, plaintext)

```json
{
  "version": 1,
  "node_id": "<hex>",
  "auth_public_key": "<hex>",
  "encryption_public_key": "<hex>"
}
```

Reads (`node inspect`, `group list` / `inspect`, `conversation log`, `cosl log`, `key list`) work locked and without `serve`. They do not prompt for a passphrase.

### 3.3 `config.toml`

```toml
headscale_url = "https://headscale.example"
display_name = "laptop"
hostname = "defios-a1b2c3d4"
```

Tailnet join uses `TS_AUTHKEY` (or `config.toml` `auth_key`, discouraged). Stock headscale: MagicDNS on, embedded DERP. Hostname default at init: `defios-` + first 8 hex chars of Node ID.

### 3.4 Vault

`filippo.io/age` `ScryptRecipient` / `ScryptIdentity`, work-factor **18**, passphrase non-empty, sole recipient. Binary age file, not armored. Inner plaintext is protobuf:

```proto
syntax = "proto3";
package defios.vault.v1;

message VaultContents {
  uint32 version = 1;                 // 1
  bytes auth_seed = 2;                // 32-byte Ed25519 seed
  bytes encryption_private_key = 3;   // 32-byte X25519
  bytes ecdsa_preparams = 4;          // opaque adapter bytes (JSON of tss-lib LocalPreParams)
  repeated KeyShare shares = 5;
}

message KeyShare {
  string curve = 1;                   // ecdsa | eddsa
  bytes public_key = 2;               // canonical export
  bytes save_data = 3;                // opaque adapter bytes (JSON of LocalPartySaveData)
}
```

`init` generates ECDSA preparams into the Vault (blocking, with a progress line on stderr). Regenerated only if missing. A share is written when the engine `Done` fires, **before** this Node appends `session.complete`. Abort or mismatch deletes that pending share.

`serve` unlocks (passphrase prompt, or `DEFIOS_PASSPHRASE`, or `--passphrase-file`). Secrets live in memory until the process exits: no idle timeout, no `mlock`.

### 3.5 Unix socket

`0600`, never bound on the tailnet. Framing is an implementation detail of this Node; it is not a public API. Same-uid access can drive this Node (that Node is lost — threshold is meant to survive that). It cannot drive a peer.

---

## 4. Transport and Raft

Decided in [Choose the COSL ordering algorithm](https://github.com/pamacure/defios/issues/3). Raft `ServerID` is Node ID hex ([Design Group formation and membership mechanics](https://github.com/pamacure/defios/issues/5)).

### 4.1 Ports on tsnet

| Port | Role |
|---|---|
| **4632** | Raft `StreamLayer` (`tsnet.Listen("tcp", ":4632")` / `tsnet.Dial`) |
| **4633** | COSL forward gate |
| **4634** | Formation HTTP (`/v1/identity`, propose/accept/decline) |

`raft.ServerAddress` is `<tsnet hostname>:4632`.

### 4.2 Raft config

| Knob | MVP value |
|---|---|
| `HeartbeatTimeout` | 500ms |
| `ElectionTimeout` | 2s (DERP-relayed paths) |
| `CommitTimeout` | 50ms |
| `SnapshotInterval` | 120s |
| `SnapshotThreshold` | 1024 |
| `TrailingLogs` | 512 |
| `Apply` timeout | 10s |

`BootstrapCluster` with the **identical** voter list (every Roster Node ID / address) on every Node after formation succeeds. Ignore the "already bootstrapped" error on retry. Do not 1-Node-then-`AddVoter`.

### 4.3 Append path

1. Author signs an envelope.
2. Leader: `Raft.Apply(envelope_bytes, 10s)`. Follower: `ForwardRequest` to `Raft.Leader()` on port 4633; leader `Apply`s.
3. Leader skips propose if `msg_id` is already committed or in-flight.
4. Commit is Raft majority of `|Roster|`. For n=2 that is both Nodes.
5. `FSM.Apply` verifies and inserts. Only `LogCommand` entries produce COSL rows; raft-internal indexes may gap.

Forward gate protobuf: `{ envelope bytes }` → `{ index, term, error }`. The gate also answers `GET /v1/applied` with the leader's applied index, so a Node coming back (§4.4) can wait until it has read its own history before appending `membership.join` again.

The local API also has `POST /v1/raft/snapshot` (no CLI verb): it snapshots and compacts now instead of waiting for §4.2's interval and threshold. Raft does this on its own; the verb exists so tests can make a Node fall beyond the retained log quickly.

### 4.4 Catch-up

Automatic. Behind within the retained log: AppendEntries backfill. Beyond it: `InstallSnapshot`; `FSM.Restore` writes a staging sqlite, verifies the `prev_hash` chain from genesis to the snapshot's recorded head and that this replica's rows are its prefix, then sqlite Online Backup API into the live file (the one legitimate rewrite; triggers do not fire). A snapshot no further along than the replica changes nothing; one that does not verify is refused and the live replica left as it was. A Node whose Vault survived and whose replica state (`cosl.sqlite`, `raft.db`, `raft-snap/`) is gone is still that Node — the leader snapshots it, and it does not append a second `membership.join`. `roster.json` must survive for that: without it the Node has no Group to start Raft for and must form again (§6). No extra resync verb. Lost Vault, or no majority with data: unrestorable at MVP.

### 4.5 Availability

n=2 appends only while both Nodes are reachable. That is inherent to crash-safe total order with two replicas and is free for 2-of-2 Sessions. n≥3 (later) tolerates `floor((n−1)/2)` offline Nodes for COSL commit; keygen still needs all n live.

---

## 5. COSL protocol

Decided in [Define the COSL message envelope and type registry](https://github.com/pamacure/defios/issues/4); why in [ADR 0001](../adr/0001-cosl-message-envelope.md). Amendments: sender is Node ID; `group_id` is derived; `conversation_id` is derived; `session.start` carries sealed nonces; routing tag is Node ID.

### 5.1 Envelope v1

```proto
syntax = "proto3";
package defios.cosl.v1;

message Envelope {
  uint32 version = 1;                 // 1
  bytes group_id = 2;                 // 32
  uint32 msg_type = 3;                // registry
  bytes sender = 4;                   // Node ID, 32
  bytes msg_id = 5;                   // 32 random, signature-covered
  optional bytes conversation_id = 6; // unset = Group-level; else 32 derived
  int64 created_at = 7;               // unix nanos, advisory, never ordering
  bytes payload = 8;                  // type-specific protobuf
  bytes sig = 9;                      // 64-byte Ed25519
}
```

No ordering metadata in the envelope. Raft imposes `(term, index)` at commit into sqlite `term` / `seq`.

**Unknown `version`:** reject (do not insert). **Unknown `msg_type`:** verify signature, insert, readers skip. An older Node that dropped unknown types would miss rows every other replica has.

### 5.2 Registry

| id | name | `conversation_id` | payload |
|---|---|---|---|
| 1 | `membership.join` | unset | [§5.3](#53-payloads) |
| 2 | `session.start` | unset | |
| 3 | `session.abort` | unset | |
| 4 | `session.complete` | unset | |
| 5 | `tss.session` | unset | |
| 6 | `app.freeform` | unset = Group-level; set = that Key's Conversation | |

No agent namespace. An agent-initiated message is an ordinary Node-authored message. Future kinds are ordinary id additions.

### 5.3 Payloads

```proto
syntax = "proto3";
package defios.cosl.v1;

message MembershipJoin {
  string display_name = 1;            // UTF-8, 1–64 chars, no newlines
  string tsnet_hostname = 2;
  bytes encryption_public_key = 3;    // 32-byte X25519
  bytes auth_public_key = 4;          // 32-byte Ed25519
}

message SealedBox {
  bytes to = 1;                       // recipient Node ID
  bytes ephemeral_public_key = 2;     // 32
  bytes nonce = 3;                    // 24
  bytes ciphertext = 4;
}

message SessionStart {
  bytes session_id = 1;               // 32
  string curve = 2;                   // ecdsa | eddsa
  uint32 t = 3;                       // DeFiOS min-signers
  uint32 n = 4;                       // |Roster|
  repeated bytes participants = 5;    // Node IDs, sorted; must equal the Roster
  repeated SealedBox nonce_boxes = 6; // one per participant
}

message SessionAbort {
  bytes session_id = 1;
  string reason = 2;
}

message SessionComplete {
  bytes session_id = 1;
  string curve = 2;
  bytes public_key = 3;               // canonical export
}

message TssSession {
  bytes session_id = 1;
  bytes routing_tag = 2;              // empty = broadcast; else recipient Node ID
  bytes ephemeral_public_key = 3;     // empty iff broadcast
  bytes nonce = 4;                    // empty iff broadcast
  bytes data = 5;                     // broadcast: engine bytes; p2p: box ciphertext
}

message AppFreeform {
  string text = 1;                    // UTF-8, max 65536 bytes
  string content_type = 2;            // default text/plain
}
```

One `tss.session` kind serves every round of both curves and both engines. Rounds self-describe inside `data`. The routing tag is engine metadata, never a delivery address: every message lands on every replica. Broadcast `data` is plaintext engine bytes. Point-to-point `data` is a sealed box of those bytes.

### 5.4 FSM apply

On each committed `LogCommand`, in order:

1. Unmarshal envelope. Fail → skip insert (raft index gaps).
2. `version == 1`. Else skip insert.
3. `group_id` equals this replica's Group ID.
4. `sender` is on the Roster.
5. Ed25519 verifies against the Roster's pinned auth public key for that Node ID, over canonical bytes with `sig` empty.
6. `INSERT OR IGNORE` on `msg_id` (duplicate occupies a raft index with no new COSL row, identically on every replica).
7. `prev_hash = SHA-256( prev.prev_hash || seq_be64 || term_be64 || envelope_bytes )`. Genesis `prev_hash` is 32 zero bytes. `seq` and `term` are 8-byte big-endian uint64. `seq` is the raft log index.

`membership.join` is additionally self-asserted (`sender` is the member recorded). It does **not** add anyone: the FSM still requires the sender already on the Roster. Session policy (who may start, t, one-open-Session) is the Session layer, not the FSM.

### 5.5 sqlite schema

```sql
PRAGMA journal_mode = WAL;
PRAGMA synchronous = FULL;
PRAGMA busy_timeout = 5000;

CREATE TABLE cosl (
  seq              INTEGER PRIMARY KEY, -- raft log index; gaps allowed
  term             INTEGER NOT NULL,
  envelope         BLOB    NOT NULL,    -- canonical signed protobuf bytes
  sender           BLOB    NOT NULL,    -- Node ID (32)
  msg_type         INTEGER NOT NULL,
  msg_id           BLOB    NOT NULL,    -- 32 random
  conversation_id  BLOB,                -- NULL = Group-level
  created_at       INTEGER NOT NULL,
  prev_hash        BLOB    NOT NULL
);
CREATE UNIQUE INDEX idx_cosl_msg_id ON cosl(msg_id);
CREATE INDEX idx_cosl_conversation ON cosl(conversation_id, seq);
CREATE INDEX idx_cosl_type ON cosl(msg_type, seq);

CREATE TABLE cosl_meta (
  key   TEXT PRIMARY KEY, -- last_applied_seq, last_applied_term, chain_head
  value BLOB NOT NULL
);

CREATE TRIGGER cosl_no_update BEFORE UPDATE ON cosl
BEGIN SELECT RAISE(ABORT, 'cosl is append-only'); END;
CREATE TRIGGER cosl_no_delete BEFORE DELETE ON cosl
BEGIN SELECT RAISE(ABORT, 'cosl is append-only'); END;
```

One writer: Raft's FSM thread. WAL readers for CLI. No `keys` table: `key list` is matching `session.complete` rows plus Vault shares. No second Conversation table: `conversation log` is `WHERE conversation_id IS ?`. Envelope bytes are the source of truth; denormalized columns are for queries.

---

## 6. Group formation

Decided in [Design Group formation and membership mechanics](https://github.com/pamacure/defios/issues/5); why in [ADR 0002](../adr/0002-group-formation.md). There is no Invite file. The tailnet is transport, not COSL authorship.

### 6.1 Identity (pre-Raft, tsnet :4634)

```proto
syntax = "proto3";
package defios.group.v1;

message Identity {
  bytes node_id = 1;
  bytes auth_public_key = 2;
  bytes encryption_public_key = 3;
  string display_name = 4;
  string tsnet_hostname = 5;
  bytes sig = 6;
}

message CreateProposal {
  bytes group_id = 1;
  repeated Identity roster = 2;       // sorted by node_id; includes initiator
  bytes initiator = 3;                // Node ID
  int64 created_at = 4;
  bytes sig = 5;                      // initiator auth
}

message CreateAccept {
  bytes group_id = 1;
  bytes node_id = 2;
  bytes sig = 3;
}

message CreateDecline {
  bytes group_id = 1;
  bytes node_id = 2;
  string reason = 3;
  bytes sig = 4;
}
```

`GET /v1/identity` returns an Identity. Fetchers check the signature and `node_id == SHA-256("defios-node-id-v1" || auth_public_key)`.

`node list` discovers tsnet peers, fetches each Identity, and prints Node ID, display name, hostname.

### 6.2 Select-and-accept

1. Initiator `group create <node-id>…` (self included; MVP: exactly one other Node, so n=2). Reject if this Node already has a valid or pending Group.
2. Compute Group ID from the selected Node IDs. Build a signed Create proposal (the structured blob a future `defios://v1/group?proposal=<base64url>` wraps — seam only, not implemented).
3. `POST /v1/group/propose` to every selected Node. Recipients verify signature, hash, that they are on the roster, and that they have no valid Group; they store it as `pending`.
4. Selected Nodes `group accept <group-id>` or `group decline <group-id>`. The initiator's create **is** their accept. Each side `POST`s Accept/Decline to every other selected Node.
5. When a Node has the proposal plus an Accept from every roster Node, it builds that identical Roster and starts Raft (`BootstrapCluster` + `NewRaft`). The Group is then **valid**. Any Decline cancels that Group ID (retryable).
6. After valid, each Node appends its own `membership.join`. That row is the durable copy for catch-up; it does not change membership.

Roster is membership; tailnet is reachability. A tailnet machine not on the Roster is ignored. A Roster Node that is offline stays on the Roster and shows unreachable. Hostname and display name may refresh from a live Identity; Node ID, auth public key, and encryption public key do not.

`group list` prints Group IDs and status (`pending` | `valid`). `group inspect` prints status and, for each Roster Node: Node ID, display name, tsnet hostname, reachable, whether `membership.join` is on the COSL, auth public key, encryption public key.

---

## 7. Keygen Session orchestration

Decided in [Design keygen Session orchestration over the COSL](https://github.com/pamacure/defios/issues/6); why in [ADR 0003](../adr/0003-keygen-session-orchestration.md). Local accept amended by [Design Node storage](https://github.com/pamacure/defios/issues/8). t-policy amended by [Design the Key-scoped group chat channel](https://github.com/pamacure/defios/issues/7).

### 7.1 Start

Any Roster Node, once the Group is valid, may `session start --curve ecdsa|eddsa --t 2`. Payload:

- Session ID (random)
- curve, t, n=`|Roster|`, participants = Roster
- 32-byte `SetSessionNonce` preimage, sealed to each participant's Encryption keypair — plaintext never a COSL field

**t validity:** `2t > n` and `t ≤ n` (2-of-2, 2-of-3, 3-of-4, …). This is Key policy, not a COSL commit rule. Out-of-range t, participants ≠ Roster, or a start while another Session is open: Session layer does not `Start()` the engine; any Node may `session.abort` that Session ID. The start row stays on the log. Two racing starts: Raft order wins; abort the later Session ID.

The engine adapter maps DeFiOS t onto tss-lib as `threshold = t − 1`. MVP acceptance promises only 2-of-2 (`t=2, n=2`); the fields stay on the wire.

The initiator's `session start` **is** that Node's local accept.

### 7.2 Join is implicit; engine Start is not

Replica apply of `session.start` is implicit join — no COSL accept type. Group membership was the gate.

Engine `Start()`, unsealing `tss.session`, and signing round envelopes wait on **local accept** so an unattended `serve` cannot rubberstamp a peer's start. Other Nodes: `serve` logs `session <id> waiting for accept` on stderr; the operator runs `session accept <id>`. Silence until the 2-minute `WaitingFor` still yields `session.abort`.

After local accept, replay committed `tss.session` rows for that Session ID in `seq` order, then consume live ones.

### 7.3 Party identity and engine seam

```go
type KeygenEngine interface {
    Start(params Params) error
    Receive(from [32]byte, isBroadcast bool, data []byte) error
    Out() <-chan Outgoing           // Data is always plaintext engine bytes
    Done() <-chan Result            // share + canonical public key, or error
    WaitingFor() [][32]byte
    Abort()
}

type Params struct {
    SessionID   [32]byte
    Curve       string              // ecdsa | eddsa
    T, N        int                 // DeFiOS min-signers, roster size
    Participants [][32]byte         // Node IDs
    Nonce       [32]byte            // unsealed preimage
    Preparams   []byte              // ECDSA only
}

type Outgoing struct {
    To          *[32]byte           // nil = broadcast
    Data        []byte
}

type Result struct {
    PublicKey []byte                // canonical export
    SaveData  []byte                // opaque, into the Vault
    Err       error
}
```

The Session layer seals `Outgoing` when `To != nil`, wraps COSL `tss.session`, and unwraps on receive. Swapping to Lockness later changes payload bytes and the adapter, not the COSL.

tss-lib adapter:

- `PartyID.Key` = `big.Int` from Node ID bytes (big-endian); `PartyID.Id` = hex(Node ID); indices from `SortPartyIDs`
- `tss.S256()` / `tss.Edwards()`
- `Parameters.SetSessionNonce` from the unsealed 32 bytes as `big.Int`
- `UpdateFromBytes(data, fromPartyID, routing_tag_empty)`
- ECDSA 4 rounds, EdDSA 3; no resume of in-memory party state

### 7.4 Rounds, drop, retry

Live COSL and `WaitingFor` exceeds **2 minutes** → any Node may abort. Crash/restart that loses party state → that Node appends `session.abort` once the COSL is writable. Retry is a new operator-initiated `session.start` with a new Session ID, never automatic. Aborted `tss.session` rows stay on the log.

At most one Session in flight per Group. Many Keys, minted in series.

### 7.5 Complete

Each participant appends `session.complete` with the canonical public key it computed, after the share is in the Vault. The Key exists iff all n completes agree **byte-for-byte** on curve + public key. Mismatch → abort, delete the local pending share. Matching completes close the Session.

### 7.6 MVP mint

Two sequential Sessions on a valid 2-Node Group: ECDSA then Ed25519, each `t=2, n=2`.

---

## 8. Conversation

Decided in [Design the Key-scoped group chat channel](https://github.com/pamacure/defios/issues/7); why in [ADR 0004](../adr/0004-conversation-on-cosl.md).

| send | `conversation_id` |
|---|---|
| `conversation send "text"` | unset (Group-level) |
| `conversation send --key <id> "text"` | derived Conversation ID |

`conversation log` / `conversation log --key <id>` read this replica, same filter. Any Roster Node may send. Membership is the Roster. A Conversation exists iff the Key exists. Encryption keypair is unused (broadcast plaintext); authorship is the envelope signature.

Catch-up is Raft. Clearing history is a local view, not a COSL delete.

---

## 9. CLI reference

Binary: `defios`. Human output by default; `--json` on any verb. IDs in hex. Global `--dir`. Exit codes: `0` ok, `1` usage, `2` operational (no `serve`, wrong dir), `3` protocol reject, `4` verify failure.

Verbs that write the COSL, unseal, or sign require `serve` (except `init`). Reads of identity and sqlite do not.

### 9.1 Node

| Command | Requires serve | Behavior |
|---|---|---|
| `init [--name] [--auth-key <32-byte-hex-seed>]` | no | Creates the device: config + data roots, mints (or imports) auth seed and Encryption keypair, writes `identity` + `vault/` + the device-level default display name, generates ECDSA preparams. Writes no `config.toml` and no Group (ADR 0007). Prompts for passphrase twice unless `DEFIOS_PASSPHRASE` / `--passphrase-file`. |
| `serve [--passphrase-file]` | — | Serves the Current Group: unlocks Vault, starts tsnet, formation HTTP, Raft if a valid Group exists. Refuses if socket bound. Errors to `group new`/`group use` if no Group / no Current Group. |
| `node inspect` | no | Node ID, display name, hostname, auth public key, encryption public key, serve up? Group ID if any. Display name/hostname come from the Current Group's config; before any `group new`, the display name is the device default and there is no hostname. |
| `node list` | yes | Discovered Identities on the Current Group's tailnet: Node ID, display name, hostname. |

### 9.2 Group

| Command | Requires serve | Behavior |
|---|---|---|
| `group new <alias> --headscale <url> [--hostname] [--name]` | no | Scaffolds a Group under a local alias (its config + data subtrees and per-Group `config.toml`) before any network, and makes it the Current Group. Alias: `^[a-z0-9][a-z0-9_-]{0,63}$`, unique on the Node, never on the wire. Hostname default `defios-<first 8 hex of Node ID>-<alias>`; display name default the `init --name` device default. MVP: at most one Group. |
| `group use <alias>` | no | Sets the Current Group (a plaintext pointer at the config root) that `serve` and read verbs default to. |
| `group create <node-id>…` | yes | Self must be included. MVP: exactly one other Node. |
| `group accept <group-id>` | yes | |
| `group decline <group-id>` | yes | |
| `group list` | no | Group IDs + `pending`/`valid`. |
| `group inspect [group-id]` | no | Roster rows as in §6.2. Default: this Node's Group. |

### 9.3 Session and Key

| Command | Requires serve | Behavior |
|---|---|---|
| `session start --curve ecdsa\|eddsa [--t 2]` | yes | n is `|Roster|`, not a flag. This Node's local accept. |
| `session accept <session-id>` | yes | Local engine gate only. |
| `session abort <session-id> [--reason]` | yes | Appends `session.abort`. |
| `session list` | no | Open / aborted / complete; Session ID, curve, t, n. |
| `session inspect <session-id>` | no | Participants, status, completes seen. |
| `key list` | no | Each minted Key: `curve:hex`, t, n, share present? |
| `key inspect <key>` | no | Canonical hex, Conversation ID, minting Session ID, t, n, share present. |

### 9.4 Conversation and COSL

| Command | Requires serve | Behavior |
|---|---|---|
| `conversation send [--key <id>] <text>` | yes | |
| `conversation log [--key <id>] [--limit N]` | no | `seq`, RFC3339 `created_at`, display name, text. |
| `cosl log [--type] [--limit N]` | no | All types; payload summarized. |
| `cosl decode <msg-id-or-hex>` | no | Full envelope + payload. |

Human `conversation log` line: `<seq> <time> <display_name>  <text>`.

Human `key list` line: `<curve:hex>  t=<t> n=<n>  share=yes|no`.

---

## 10. Threat model

**In:**

- Crash-fail Nodes; catch-up on reconnect (Raft).
- Sender authentication: every applied COSL row verifies Ed25519 against the Roster's pinned auth public key for that Node ID.
- Tailnet is transport (WireGuard in flight). It is not COSL authorship — forwarding and at-rest replicas would otherwise impersonate.
- Point-to-point Session payloads sealed, so a stolen COSL file cannot reassemble both halves of a 2-of-2 Key.
- Vault passphrase gates secrets at rest.
- Local accept so an unlocked `serve` does not mint Keys unattended.

**Honest-operator (documented non-goal to defend):** a Roster Node that equivocates, a dishonest Raft leader that reorders or delays, or a Node that withholds. Signatures + `prev_hash` are tamper *evidence*, not byzantine defense.

**Out of scope at MVP** (also [Out of scope](https://github.com/pamacure/defios/issues/1) on the map):

- Threshold signing and chain integration.
- Agents and rule engines.
- Mobile as a supported target.
- QR/URL join; an Invite file.
- Headscale server-side; custom DERP.
- Any UI beyond the CLI.
- Key resharing, refresh, rotation; membership changes; auth-key or Encryption-keypair rotation (seams only).
- Byzantine-safe ordering.
- More than one Group per Node.
- Clearing Conversation history as a COSL delete.
- OS keyring / passkey / biometric as the Vault mechanism.

---

## 11. Acceptance

### 11.1 2-of-2 keygen, both curves

On a valid 2-Node Group, two sequential Sessions (`ecdsa` then `eddsa`, each `t=2, n=2`):

- Each COSL replica has one `session.start` and two `session.complete`s for that Session ID.
- The two completes agree on canonical public-key bytes.
- Each Vault holds a share for that Key (`key inspect` → `share=yes` on both Nodes).
- Both Nodes print the same `curve:hex` from `key list`.

### 11.2 COSL convergence

After those Sessions (and at least one Group-level `app.freeform`):

- Both replicas have the same set of `msg_id`s in the same `seq` order for COSL rows.
- `prev_hash` chains match at the head.
- `cosl log` from both Nodes is identical for the committed prefix.

A 2-Node Group does not append while one Node is down; when it returns, InstallSnapshot / log backfill restores the prefix without a manual resync verb.

### 11.3 Auth-verification negatives

FSM `Apply` (unit tests; no tailnet required) **does not insert** when:

| Case | Result |
|---|---|
| `sig` does not verify under the Roster key for `sender` | skip |
| `sender` not on the Roster | skip |
| `group_id` mismatch | skip |
| unknown `version` | skip |
| truncated / unparseable envelope | skip |

And **does insert** when `msg_type` is unknown but the envelope otherwise verifies (readers skip). Duplicate `msg_id`: no second COSL row.

A Node ID advertised with a non-matching auth public key (`node_id ≠ SHA-256("defios-node-id-v1" || pk)`) is rejected at Identity fetch and does not enter a Create proposal.

### 11.4 Local accept

A `session.start` from the other Node lands on the replica. This Node does not emit `tss.session` and does not `Start()` until `session accept`. With no accept, the Session aborts by the 2-minute `WaitingFor`.

### 11.5 Formation

Two Nodes on one headscale tailnet: `node list` sees the other Identity; `group create` + `group accept` yields `valid`; both append `membership.join`; `group inspect` agrees on Roster and Group ID.

---

## 12. Test harness

Package `e2e`, build tag `e2e` (`go test -tags e2e ./e2e`). One Go test binary, two in-process Nodes (separate temp `--dir` trees, two `tsnet.Server`s), dockerized stock headscale via testcontainers (MagicDNS on, embedded DERP, two preauth keys). CI: GitHub Actions on Linux. macOS is a supported runtime; it is not the CI OS.

Tests map 1:1 onto [§11](#11-acceptance):

- `TestFormation`
- `TestKeygenECDSA_2of2`
- `TestKeygenEd25519_2of2` (after ECDSA, same Group)
- `TestCOSLConvergence`
- `TestAuthVerificationNegatives` (may live in `internal/cosl` without Docker)
- `TestSessionLocalAccept`

Drive Nodes through the same functions the CLI calls, not by scraping TTY. Passphrases via `DEFIOS_PASSPHRASE`.

---

## 13. Future work

Recorded as seams, not designs.

**Signing.** First post-MVP milestone. A signing Session reuses COSL `session.*` / `tss.session`, Key identity, Vault shares, and local accept. It is the first protocol type that would set `conversation_id` on a non-freeform message. t remains how many shares make a Key signature.

**Engine swap.** Adopt Lockness `cggmp24` + `givre` via wazero+wasm once cggmp24 leaves 0.7-alpha (and ideally an audit refresh). givre's FROST layer is unaudited. Envelope already engine-agnostic. Findings: `docs/research/tss-engine-lockness-rust.md` on branch `research/tss-engine-lockness-rust`.

**Chains.** Bitcoin / Ethereum / Solana integration sits on canonical public-key export ([§2.4](#24-key-identity-and-canonical-public-keys)) plus signing Sessions. Not in this spec.

**Agents.** Agent-initiated messages are Node-authored COSL rows; no reserved type range. Rule engines later.

**Mobile.** Out of MVP. tsnet compiles for android/arm64 (no cgo) and iOS (cgo/Xcode) and ships in the official apps via libtailscale, but open blockers remain: Android app-sandbox netlink denial (`tailscale#17311`), Android TLS cert stub (`#21719`), iOS suspension reclaiming sockets (`#20060`, `#21353`). A phone Node is not always reachable — crash-tolerant COSL already matches that. Neither Android Keystore nor Secure Enclave accepts Ed25519; a mobile auth keypair is software-generated, biometric-gated encrypted bytes. Leave-room: userspace tsnet (no VPN entitlement), Vault behind a per-Node storage seam. Findings: `docs/research/tsnet-mobile-feasibility.md` on branch `research/tsnet-mobile-feasibility`.

**QR join.** Create proposal is already a signed structured blob. A future `defios://v1/group?proposal=<base64url>` wraps it (desktop creates, phone scans). No Invite file.

---

## 14. Sources

Decisions this spec assembles, by name:

- [Catalog what LFDT-Lockness tss-lib gives us for ECDSA and Ed25519 keygen](https://github.com/pamacure/defios/issues/2)
- [Choose the COSL ordering algorithm](https://github.com/pamacure/defios/issues/3)
- [Define the COSL message envelope and type registry](https://github.com/pamacure/defios/issues/4) — [ADR 0001](../adr/0001-cosl-message-envelope.md)
- [Design Group formation and membership mechanics](https://github.com/pamacure/defios/issues/5) — [ADR 0002](../adr/0002-group-formation.md)
- [Design keygen Session orchestration over the COSL](https://github.com/pamacure/defios/issues/6) — [ADR 0003](../adr/0003-keygen-session-orchestration.md)
- [Design the Key-scoped group chat channel](https://github.com/pamacure/defios/issues/7) — [ADR 0004](../adr/0004-conversation-on-cosl.md)
- [Design Node storage: encrypted keyshare container and COSL sqlite schema](https://github.com/pamacure/defios/issues/8) — [ADR 0005](../adr/0005-node-storage-vault-and-cosl-sqlite.md)
- [Verify what tsnet supports on mobile today](https://github.com/pamacure/defios/issues/9)
- [Verify the Lockness Rust stack as DeFiOS's TSS engine](https://github.com/pamacure/defios/issues/11)
