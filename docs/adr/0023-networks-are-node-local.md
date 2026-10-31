# Networks are Node-local; a Sign Request names its chain

[ADR 0008](0008-networks-on-cosl.md) put Networks on the COSL so that every
Node agrees on what a Network is before it signs for it. A Sign Request's
transactions pinned a `network.create` / `network.update` row by its
`msg_id`, and every Node read the chain id (or Bitcoin network) from that row
to derive the Digests. Under the trust model of
[ADR 0022](0022-members-are-trusted-for-their-own-messages.md) a shared
registry is one member deciding for every Node which chains exist, and the
agreement it bought belongs in the request itself. Each transaction of a
Sign Request now carries its chain's **identity**: Ethereum's `chain_id`,
Bitcoin's `network`, Solana's `cluster` and optional `genesis_hash`. Every
Node derives the Digests from the request alone. A Network is this Node's
own `[networks.<alias>]` table: the chains it is willing to sign for, and the
endpoints it uses to reach them. This is pre-release, so nothing is
migrated.

## Considered options

- **Keep Networks on the COSL** (ADR 0008). Rejected: two Nodes never need
  to agree on more than what is signed, and the request now says that. The
  rest of a Network (its endpoints, its alias) is how one Node reaches a
  chain, and the shared `rpc_urls` were already a shared input to whatever a
  Node later reads from the chain.
- **Carry the whole Network config in the request.** Rejected: endpoints and
  aliases mean nothing to another Node. Only the fields that change what is
  signed, or which chain a signature is good on, ride the request.
- **Let a Node accept a request for a chain it has not configured.**
  Rejected: a Node's Networks are its allowlist of chains. A request for any
  other chain is one it cannot check against a chain or broadcast, so it is
  refused.
- **Endpoints as tables with headers** (`[[networks.<alias>.endpoints]]`
  with `url` and `headers = { Authorization = "$NAME" }`), for endpoints
  that take a key in a header. Deferred: every endpoint we expect to use
  takes its key in the URL or as basic auth. This is the shape to add when
  one does not.

## Consequences

- **`SignTransaction`** replaces `network_config_id` with the identity of
  its chain, by type: `EthereumIntent` gains `chain_id` (> 0), `BitcoinIntent`
  gains `network` (`mainnet | testnet | signet | regtest`), and `SolanaIntent`
  gains `cluster` (`mainnet-beta | devnet | testnet | localnet`) and an
  optional `genesis_hash`. The Digest rules are unchanged; they take the
  chain id and Bitcoin network from the intent. Bitcoin's network is not in
  its sighash, but it decides how output addresses decode, so it is signed
  for all the same.
- **Proposing.** `sign request --network <alias>` resolves a local alias to
  its identity when the request is written, and checks the Key's curve
  against its type as before. The request shows the chain (`ethereum chain
  1`), not anyone's alias.
- **Accepting.** A Node refuses to accept a request whose transactions name
  a chain none of its Networks has. Before it acts on a chain, by reading it
  or broadcasting to it, a Node may check that its endpoint serves the chain
  the request names (`eth_chainId`, Solana's genesis hash, Bitcoin Core's
  chain).
- **`[networks.<alias>]`** in a Workspace's `config.toml` is the whole
  Network: `type`, the identity fields of that type, and `rpc_urls`. Two
  aliases may not name the same chain. A Conversation's `config.toml`
  overrides it table by table as before.
- **An `rpc_urls` entry is a plain URL or a Secret.** A plain entry is an
  `http(s)` URL with no userinfo and no query, for an endpoint that needs no
  credential. Anything that carries one (a key in the path or query, or
  basic auth as `https://user:pass@host`) is a `"$NAME"` reference to a
  Secret holding the full URL ([ADR 0013](0013-group-config-workspaces-and-named-secrets.md)).
  A plain URL with userinfo or a query is refused when the file is read. A
  key in a path cannot be told from a path, so it is the operator's to put
  in a Secret.
- **`sign.broadcast`.** A `BroadcastedTx` drops `network_config_id`: its
  `tx_index` already names the transaction, and with it the chain. A
  receipt still covers the transactions of one chain, and readers drop one
  that mixes chains. Transactions are grouped for broadcast by chain identity.
- **The `network.*` registry ids (8, 9, 10) are retired** and stay reserved.
  `network add` and friends write the local table.
