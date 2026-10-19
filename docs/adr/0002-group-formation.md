# Node ID derived once from the auth public key; Groups form by select-and-accept on the tailnet

A Node's public-facing identity is not its auth public key: Node ID is
`SHA-256("defios-node-id-v1" || auth_public_key)`, computed once at init and
stored so a later auth-key rotation (a COSL message signed by the previous key)
does not rename the Node. Group ID is `SHA-256("defios-group-id-v1" || sorted
Node IDs)`, so one Roster is one Group. Formation is in-band: signed Identities
on an existing tailnet, the initiator selects Node IDs, every selected Node
accepts, then each Node starts Raft with that identical Roster. Decided in
[defios#5](https://github.com/pamacure/defios/issues/5).

## Considered options

- **Node ID = auth public key** — rejected: name and verify are different jobs.
  Reusing the key as the name collapses rotation and restore into one object and
  overloads every protocol field.
- **Node ID = SHA-256(auth private key)** — rejected: other Nodes cannot check
  the binding, so an attacker can advertise a known Node ID with their own
  signing key. Hashing the secret into a published value is also a footgun.
  Impersonation is a signature problem, not a naming problem.
- **Random minted Node ID** — rejected: restore from a backed-up auth key must
  reproduce the same Node ID without a separate identity backup.
- **Out-of-band Invite file + Join-receipt** — rejected once Nodes on the same
  tailnet already know each other's Identities. The remaining out-of-band step
  is joining the tailnet. A Create proposal stays a structured blob a future
  `defios://` URL can wrap.
- **1-Node Raft then AddVoter** — already rejected in
  [Choose the COSL ordering algorithm](https://github.com/pamacure/defios/issues/3);
  this ticket keeps the identical-Roster start.

## Consequences

- COSL `sender` and Raft `ServerID` are Node ID.
  `membership.join` carries the auth public key — an amendment to
  [defios#4](https://github.com/pamacure/defios/issues/4).
- Node storage persists Node ID in the plaintext identity file, written once
  at init and never recomputed, beside the Vault that holds both private keys
  and keyshares
  ([Design Node storage: encrypted keyshare container and COSL sqlite schema](https://github.com/pamacure/defios/issues/8);
  [ADR 0005](0005-node-storage-vault-and-cosl-sqlite.md)).
- Session `PartyID` is keyed by Node ID
  ([Design keygen Session orchestration over the COSL](https://github.com/pamacure/defios/issues/6);
  [ADR 0003](0003-keygen-session-orchestration.md)).
- `group list` is Group IDs only; the Roster lives on `group inspect`.

