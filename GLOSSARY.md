# DeFiOS

A self-hosted multi-party computation wallet: Nodes form a Group that jointly custodies threshold Keys and coordinates over the COSL. A Group's Nodes may all be one operator's own devices or may belong to different parties sharing custody, and one Node may belong to several Groups at once, each on its own tailnet.

## Language

**Group**:
The cohort of Nodes jointly custodying one or more threshold Keys.
_Avoid_: network, cluster, cohort, mesh

**Group ID**:
A Group's 32-byte identity, derived from the Node IDs that constitute it, so one set of Nodes is one Group.
_Avoid_: network id, cluster id

**Roster**:
The Group's set of Nodes, fixed at formation.
_Avoid_: member list, peer list, cluster membership

**Group alias**:
A local, human-chosen label a Node gives one of its Groups (e.g. `personal`, `custody-with-b`), used to name and select that Group on this machine — before the Group ID exists, and ever after. Purely local: it never appears on the wire, and two Nodes may label the same Group differently.
_Avoid_: group name, label, nickname

**Current Group**:
The one of a Node's Groups currently in focus: the Group a running serve serves, and the one local commands act on when none is named. A Node focuses one Group at a time; switching focus stops serving one Group and starts the other.
_Avoid_: active group, default group, selected group

**Node**:
One device running one DeFiOS CLI instance, holding one auth keypair. It may belong to one or more Groups, presenting that same identity in each; it exists before it does.
_Avoid_: peer, member, client

**Node ID**:
A Node's 32-byte public-facing identity, distinct from its auth public key. Determined once at init from that auth public key, then stored; it does not change if the auth keypair later does.
_Avoid_: peer id, member id, node key, auth public key

**Identity**:
A Node's signed advertisement of Node ID, auth public key, encryption public key, display name, and tsnet hostname, published on the tailnet for discovery and Group formation.
_Avoid_: profile, peer record, hello

**Auth keypair**:
A per-Node Ed25519 keypair, held by that Node alone, used to sign the messages it sends. Other Nodes verify against the auth public key pinned for that Node ID on the Roster. Unrelated to a Group's threshold Keys, even when those are also Ed25519.
_Avoid_: device key, identity key, node key

**Encryption keypair**:
A per-Node X25519 keypair, held by that Node alone, used to seal point-to-point Session payloads so only the addressed Node can read them. Unrelated to the auth keypair, which signs but never encrypts. Not an identity; the same keypair is used until it is lost or compromised.
_Avoid_: encryption key, box key, crypto key

**Vault**:
This Node's passphrase-encrypted store of secrets, and the only thing a Node keeps encrypted at rest: the auth private key, the encryption private key, and ECDSA preparams shared across all the Node's Groups, together with this Node's shares of each Group's Keys held separately per Group. Everything else — Identity, Group and Roster records, tailnet config, and the COSL replica — is plaintext, so a Group that is not currently being served can still be inspected.
_Avoid_: keystore, wallet, keyring, container

**Key**:
One threshold public key held jointly by a Group, together with the per-Node shares of it. Identified by its curve and that public key, not by the Session that minted it; exists for ECDSA or Ed25519 signing.
_Avoid_: keypair, wallet, wallet key

**Session**:
One run of a threshold protocol (keygen or signing) coordinated among a Group's Nodes over the COSL. A signing Session signs one Sign Request's Digests; it completes as soon as a signature verifies against the Key, not when every Node agrees.
_Avoid_: ceremony, run

**Signer**:
One of the t Nodes that run a signing Session: the proposer plus the first t−1 others to append a Sign Accept, fixed by the COSL's log order. Each signer's engine runs only after that Node accepted.
_Avoid_: participant (that is every Node in a keygen Session)

**Session ID**:
A Session's 32-byte identity, minted at random by the initiator when the Session starts. Identifies the run, not the Key it may mint.
_Avoid_: execution id, ceremony id, run id

**COSL**:
The canonically-ordered signature log: the shared, append-only message history every Node maintains a local replica of. Message data is signed by the sender's auth keypair.
_Avoid_: log, ledger, history, message list

**Conversation**:
A Key-scoped discussion among a Group's Nodes, carried as COSL messages whose conversation id is that Key's. One Conversation per Key; membership is the Roster. Exists iff the Key exists.
_Avoid_: chat, channel, room, thread

**Conversation ID**:
A Conversation's 32-byte identity, derived as SHA-256("defios-conversation-id-v1" || curve || joint public key). Identical to the Key's identity, hashed; not minted, not a Session ID. A nil conversation id on a COSL message marks Group-level talk, not a Conversation.
_Avoid_: chat id, channel id, thread id

**Recap**:
A COSL message (`app.recap`) carrying one Node's account of a Conversation's talk up to a stated seq, which stands in place of that talk for readers. The newest Recap for a Conversation is the live one; earlier Recaps stay on the log and keep their text.
_Avoid_: summary (that is the one-line payload line `cosl log` prints), digest, compaction, checkpoint

**Pruning**:
One Node's destruction of the `app.freeform` payloads a Recap has already accounted for, keeping each row, its hash, and its place in the chain. Node-local and written nowhere on the COSL, so two replicas of one Group legitimately hold different amounts of talk.
_Avoid_: tombstone (that is a Network removal marker, which destroys nothing), delete, compaction, garbage collection

**Network**:
A blockchain a Group may broadcast a signed transaction to, described by a config the Group maintains on its COSL and recalls by a local alias (e.g. `ethereum-mainnet`). Each Network is of one type — Bitcoin, Ethereum, or Solana — which fixes the curve a signing Key must use for it.
_Avoid_: chain config, blockchain configuration, RPC, endpoint

**Model**:
A language model a Group's agent may use, described by a config the Group maintains on its COSL and recalls by a local alias, as a Network is: a provider (Anthropic or OpenAI, fixed for the alias's life), the provider's model id, and optionally an endpoint. Credentials for it never ride the COSL.
_Avoid_: LLM config, provider (that is one field of a Model), engine (that is a threshold-signing engine)

**Sign Request**:
One Node's proposal that a Group's Key sign one or more transactions, scoped to that Key and carrying a natural-language reason. Its transactions may target several Networks of the Key's curve, but each signed transaction is broadcastable on exactly one Network. The proposing Node's signature on the request is its own acceptance. A request's status is `open`, `waiting` (t accepts, no Session yet), `signing`, `signed`, `aborted` or `expired`.
_Avoid_: transaction request, signing job, order

**Sign Accept**:
A Node's COSL message (`sign.accept`) agreeing to sign one Sign Request. The first t Nodes to accept, the proposer counting first, are its Signers; once they exist one of them starts the signing Session. Later accepts do not count, and a late `sign accept` fails with `signer-set-full`.
_Avoid_: approval, vote, signature (a Sign Accept is not a signature over a Digest)

**Digest**:
The exact bytes a Key signs for one transaction — one per Bitcoin input, one per Ethereum or single-signer Solana transaction (the keccak256 of the Ethereum transaction, the compiled message itself for Solana, which Ed25519 signs unhashed). Every Node derives the same Digests from a Sign Request independently.
_Avoid_: hash, message, sighash

**Broadcast Receipt**:
A Node's record on the COSL that it submitted signed transactions to one Network, carrying the resulting transaction hash and request position of each (`sign.broadcast`); a request spanning several Networks gets one receipt per Network that went out, and is fully broadcast once every transaction appears in some receipt; its sender is the broadcaster and its timestamp the broadcast time, so broadcasting is provable and visible to the Group. Any Node may broadcast and record one; a repeat broadcast is harmless and simply records another.
_Avoid_: confirmation, txid record, acknowledgement
