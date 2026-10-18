# DeFiOS

A self-hosted multi-party computation wallet: a person's devices form a Group that jointly custodies threshold Keys and coordinates over the COSL.

## Language

**Group**:
The cohort of Nodes jointly custodying one or more threshold Keys.
_Avoid_: network, cluster, cohort, mesh

**Node**:
One device running one DeFiOS CLI instance, a member of a Group.
_Avoid_: peer, member, client

**Auth keypair**:
A per-Node Ed25519 keypair, held by that Node alone, used to sign the messages it sends; peers verify against the known auth public keys. Unrelated to a Group's threshold Keys, even when those are also Ed25519.
_Avoid_: device key, identity key, node key

**Key**:
One threshold public key held jointly by a Group, together with the per-Node shares of it. Minted by a keygen Session; exists for ECDSA or Ed25519 signing.
_Avoid_: keypair, wallet, wallet key

**Session**:
One run of a threshold protocol (keygen, later signing) coordinated among a Group's Nodes over the COSL.
_Avoid_: ceremony, run

**COSL**:
The canonically-ordered signature log: the shared, append-only message history every Node maintains a local replica of. Message data is signed by the sender's auth keypair.
_Avoid_: log, ledger, history, message list