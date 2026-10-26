# Threshold signing stays on tss-lib; Taproot is deferred, not adopted

DeFiOS keeps `bnb-chain/tss-lib` (Go) as its only threshold signing engine and
does **not** migrate to the LFDT-Lockness Rust stack (`cggmp24` for ECDSA,
`givre` for FROST Schnorr). P2TR therefore stays unsupported: a Key remains
P2WPKH-only, as [ADR 0010](0010-signing-session-semantics.md) records. This is a
**"not now"** with explicit reopening triggers, not a rejection on principle.
The `session.Engine` seam ([ADR 0003](0003-keygen-session-orchestration.md)) is
unchanged and still the place a Lockness adapter would land. Research with
sources in the
[Lockness research note](../research/lockness-threshold-signing.md); decided in
[#44](https://github.com/pamacure/defios/issues/44), building on
[#11](https://github.com/pamacure/defios/issues/11).

The forcing question was narrow: BIP-340 Schnorr is the one capability
`tss-lib`'s ECDSA engine cannot provide, and `givre`'s `ciphersuite-bitcoin`
(verified, BIP-340 compliant) can. The decision was taken as all-or-nothing —
full migration or none — on grounds of tidiness, maintainability, and build
simplicity.

## Considered options

- **Full migration to Lockness** — rejected. It would move ECDSA signing —
  Ethereum and Bitcoin, the paths already verified byte-for-byte against btcd —
  off publicly-attacked-and-hardened Go code onto `cggmp24` **0.7.0-alpha**
  (alpha.3 published broken), whose Kudelski audit covers the **predecessor**
  CGGMP21 codebase rather than what ships, whose **t-of-n protocol is the
  authors' own unpublished CGGMP24-like design** (the paper covers only
  n-out-of-n, and DeFiOS is t-of-n), and which declares timing attacks out of
  scope. `givre`'s FROST **signing** layer has no public audit at all. The
  capability gained is Taproot alone. Every cost — Paillier safe-prime aux-info,
  the LGPL-or-slow bigint backend choice, the alpha status — sits in the
  `cggmp24` half, which is also the half that buys nothing new.
- **Partial adoption: `givre` for Taproot only, `tss-lib` retained for ECDSA and
  EdDSA** — technically the cheapest route to the capability (`givre`'s Bitcoin
  and Ed25519 ciphersuites use no Paillier), and the seam supports it. Rejected
  by the decision-maker as all-or-nothing: two MPC engines in one Group means
  two share formats, two toolchains, and a `t`-semantics translation (Lockness
  `t` = minimum signers; tss-lib `t` = signers−1), which costs more in tidiness,
  maintainability, and build simplicity than Taproot is currently worth.
- **Treating the newer library as the more secure one** — rejected as a premise.
  Release recency is not audit currency. `tss-lib`'s Kudelski report is October
  2019 and stale as a document, but the library has since been attacked in
  public by multiple independent teams — Verichains' TSSHOCK, Fireblocks,
  CVE-2022-47931 — and hardened in response (v2.0.0; DeFiOS runs v4 at a
  2026-09-23 commit). Lockness offers a newer audit of code that is not the code
  that would run, plus an unaudited signing layer.
- **cgo over a Rust staticlib, rather than wasm** — not reached, but recorded:
  it buys near-native speed at the cost of pure-Go cross-compilation, a Rust
  toolchain in every build, and crash isolation (a Rust panic takes the process
  down). wazero + wasm remains the designated route if migration is ever
  revisited, per #11; `round-based`'s `state-machine` feature gives the sync
  pull/push loop a non-async Go host needs, at roughly **4.7–4.8x native**.
- **Waiting on a Taproot adoption signal instead of deciding** — rejected as a
  way to decide, but the data informs the trigger below. Published figures are
  unreliable (Glassnode's Taproot adoption metric read 28.8% in March 2026 and
  4.4% in July 2026) and dominated by inscription dust rather than payments.
  P2WPKH remains the 2026 default with wider wallet receive support.

## Consequences

- **No new engine, no new dependency, no Rust in the build.** One language, one
  toolchain, pure-Go cross-compilation for Linux and macOS, and the mobile
  trajectory stays as #11 left it. `session.Params.Preparams` keeps its current
  tss-lib meaning.
- **A Key stays P2WPKH-only.** A request spending any other script type is still
  refused at propose and again by every Node deriving Digests, unchanged from
  [ADR 0010](0010-signing-session-semantics.md).
- **Paying Taproot is unaffected.** Outputs may still pay any address type
  including P2TR (`signreq.OutputScript`). The gap is only *receiving* at a
  `bc1p` address, so no counterparty is unreachable.
- **The `session.Engine` seam keeps earning its keep.** It is not removed or
  simplified on the strength of this decision; it remains the designated
  insertion point for a Lockness adapter, and any future engine must translate
  threshold semantics at that boundary rather than leaking engine-specific
  `t`/`n` into `session.Params`.
- **Greenfield status is what makes this cheap to revisit.** No users, keys,
  funds, or tagged releases exist, so there is no migration debt accruing
  against this choice — but equally no forcing function, and the Lockness stack
  will be materially more mature at the next review.

## Reopening triggers

Any of the following should reopen the question. The first two are the
substantive blockers; the third is a product signal that would justify paying a
higher price.

1. **`cggmp24` reaches a stable 1.0** (out of alpha) **and** carries an audit
   scoped to the shipped code, explicitly covering the t-of-n protocol.
2. **`givre`'s FROST signing layer gets a public audit** — the inherited DKG
   audit does not count.
3. **A concrete user or counterparty requirement to receive at a Taproot
   address.**

A fourth, which matters only once (1) and (2) hold: **a measured wazero bigint
benchmark** showing the Paillier aux-info round is tolerable under wasm, since
no published benchmark isolates that workload today.
