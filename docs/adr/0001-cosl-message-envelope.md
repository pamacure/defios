# COSL message envelope: versioned protobuf, random msg_id, sealed p2p payloads, no agent namespace

The COSL carries all Key-related communication, and every Node stores every message forever
in its sqlite replica — so the envelope's bytes are chosen once, and reversing them means
re-signing history. Decided in [defios#4](https://github.com/pamacure/defios/issues/4): a
versioned protobuf envelope signed by the sender's auth keypair (Ed25519 over the canonical
serialization with `sig` unset), carrying **no ordering metadata at all** — Raft imposes
the total order as `(term, index)` at commit, so a Node signs once and any Node can forward
its envelope on its behalf
([Choose the COSL ordering algorithm](https://github.com/pamacure/defios/issues/3)).

## Considered options

- **Serialization: protobuf** over JSON and CBOR. Canonical JSON needs RFC 8785 and is
  the classic key-order/number-format footgun, at 3–10× the row size; CBOR is viable but
  adds a dependency with no schema tooling. Protobuf is canonical by construction
  (field-number order, minimal varints, no maps in the schema), and
  `google.golang.org/protobuf` is already in the tree via tss-lib. Raw sqlite rows stay
  human-readable through a CLI decode command.
- **`msg_id`: 32 random bytes minted by the author**, not a content hash. Ed25519
  signatures are deterministic, so identical re-appends (a freeform "ok" sent twice)
  would produce identical content-hash ids and the second would vanish into the unique
  index. Random ids still dedup at-least-once forwarding by exact id, and are unforgeable
  because the signature covers them.
- **`created_at` is advisory only** (display), never an ordering input: clocks skew, two
  Nodes can append in the same nanosecond, and a Node can sign offline and append later.
  Canonical order is Raft's `seq`, period.
- **`tss.session` routing tag**: tss-lib's `UpdateFromBytes(data, from, isBroadcast)`
  needs broadcast-vs-p2p per message and the wire bytes don't carry it. The tag is engine
  metadata, never a delivery address: every message lands on every Node's replica — the
  COSL has no direct-message channel — but point-to-point round data is *content* for one
  Node, sealed.
- **Sealing: per-message ephemeral X25519 + a per-Node static X25519 Encryption keypair**
  (NaCl box). WireGuard encrypts only in flight; the replicas at rest are plaintext, and
  without sealing one stolen Node's disk reassembles *both* halves of a 2-of-2 Key from
  its own COSL file. Rejected: doing nothing (at-rest threshold collapse), static-to-static
  sealing (no forward secrecy on an append-only log that lives forever), and deriving the
  encryption key from the auth keypair (couples the signing and DH key domains; the
  Ed25519→X25519 conversion is exactly the subtle crypto we don't want to hand-roll).
- **No agent namespace**: an agent-initiated message is an ordinary Node-authored message,
  optionally scoped to a Conversation via `conversation_id`; future agent kinds are plain
  registry additions. A reserved id range would imply agents authenticate as something
  other than Nodes — they don't.
- **Unknown `msg_type`: verify and store, readers skip** — refusing would make an older
  Node's replica miss rows every other Node has, breaking identical total order. **Unknown
  envelope `version`: reject** — unparseable is unverifiable.

## Consequences

- The roster/invite must carry `group_id` (minted at Group formation) and each Node's
  X25519 encryption public key
  ([Design Group formation and membership mechanics](https://github.com/pamacure/defios/issues/5)).
- Node storage keeps the encryption private key alongside the keyshare, encrypted at rest;
  the sqlite row exposes `conversation_id` and `created_at` for queries
  ([Design Node storage: encrypted keyshare container and COSL sqlite schema](https://github.com/pamacure/defios/issues/8)).
- Session orchestration drives the engine through the wrapper (`from` = sender → PartyID,
  routing tag → `isBroadcast`) and keeps the `SetSessionNonce` value off the log
  ([Design keygen Session orchestration over the COSL](https://github.com/pamacure/defios/issues/6)).
- Conversations mint `conversation_id` and their COSL messages link back to it
  ([Design the Key-scoped group chat channel](https://github.com/pamacure/defios/issues/7)).
- Sealed rows are opaque in exports and debugging; only the addressed Node can open them.