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
One run of a threshold protocol (keygen, later signing) coordinated among a Group's Nodes over the COSL.
_Avoid_: ceremony, run

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
