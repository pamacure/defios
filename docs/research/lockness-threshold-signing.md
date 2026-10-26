# LFDT-Lockness as a threshold signing engine: research note

Research for [#44](https://github.com/pamacure/defios/issues/44), October 2026.
Builds on the earlier evaluation in
[#11](https://github.com/pamacure/defios/issues/11), which first catalogued the
Lockness stack and recommended the staged blend DeFiOS ships today. The decision
this note supports is recorded in
[ADR 0011](../adr/0011-threshold-signing-engine-stays-on-tss-lib.md).

The question: should DeFiOS replace `bnb-chain/tss-lib` (Go) with the
LFDT-Lockness Rust crates, in order to gain BIP-340 Schnorr and therefore P2TR?
The user framed it as all-or-nothing — full migration, or none.

## What the Lockness stack actually provides

Verified against the crates' own docs and repositories, October 2026.

| Crate | Role | Version | Audit |
| --- | --- | --- | --- |
| [`givre`](https://github.com/LFDT-Lockness/givre) | Threshold Schnorr (FROST, IETF draft) | 0.3.0, 16 Oct 2026, MIT/Apache-2.0 | **None public** |
| [`cggmp24`](https://github.com/LFDT-Lockness/cggmp21) | Threshold ECDSA | **0.7.0-alpha** (alpha.3 published broken) | Kudelski, but on predecessor code |
| `round-based` | Protocol framework (Stream/Sink) | 0.4 | n/a |
| `key-share` | Shared share types, bounds | — | covered by cggmp24's |

### `givre` ciphersuites — the P2TR premise holds

`givre`'s ciphersuite module has three feature-gated suites:

- `ciphersuite-secp256k1` — FROST over secp256k1 with SHA-256
- `ciphersuite-ed25519` — FROST over Ed25519 with SHA-512, Ed25519-compliant
- **`ciphersuite-bitcoin` — BIP-340 compliant**, the Schnorr standard Taproot uses

So the premise of #44 is confirmed: `givre` can produce the signature
`tss-lib`'s ECDSA engine cannot. `givre`'s DKG is a re-export of
`cggmp21-keygen`, generic over curve, so one DKG serves all three suites.

### Threshold semantics differ from tss-lib

Lockness `t` = **minimum number of cooperating signers**. `tss-lib`'s `t` =
**signers minus one**. Bounds are enforced at the type level by `key-share`:
`n >= 2`, `2 <= t <= n`. Any adapter must translate; `session.Params` must not
carry engine-specific `t`/`n` semantics. (First flagged in #11.)

## Concern 1: Go ↔ Rust integration and performance

DeFiOS is on `tss-lib/v4` at commit `f204718ae595` (2026-09-23) and Go 1.27.1 —
the Go side is current, not legacy.

#11 already rejected **uniffi** (no first-class Go bindings — Kotlin/Swift/
Python/Ruby plus third-party Go) and a **sidecar process** (deployment and
crash-isolation cost). That leaves two boundaries:

**wazero + wasm.** Both crates support `wasm32-unknown-unknown` (no_std), and
`round-based`'s `state-machine` feature exposes exactly the sync pull/push loop
a non-async host needs. Keeps a single static Go binary, zero cgo, free
cross-compilation, and the same artifact for Linux/macOS and a future mobile
target. The cost is speed: Frank Denis's runtime benchmarks put wazero at
**~4.7–4.8x native** as of June 2026, essentially flat since 2024, with
CPU-heavy cryptography called out as where runtime choice still matters. No
published benchmark isolates bigint under wazero, so this multiplier is an
inference for our workload, not a measurement.

**cgo + C ABI over a Rust staticlib.** Near-native speed, but surrenders pure-Go
cross-compilation, adds a Rust toolchain to every build, and lets a Rust panic
take down the process.

Three factors sharpen the performance question beyond a flat multiplier:

- **Paillier safe-prime generation.** `cggmp24`'s ECDSA path needs an
  interactive aux-info round built on Paillier safe primes — the most expensive
  operation in the stack, and bigint-bound, which is precisely the workload that
  degrades worst under wasm. Mitigable by pre-generation; the seam already
  exists as `session.Params.Preparams`, used today for tss-lib's preparams.
- **`backend-rug` is LGPL.** `cggmp24`'s fast bigint backend carries an LGPL
  dependency. The default `num-bigint` is slower. So the wasm penalty compounds
  with being on the slow backend unless the project takes on LGPL.
- **The cost is asymmetric.** `givre`'s Ed25519 and Bitcoin ciphersuites involve
  no Paillier at all. Every expensive and risky part of a migration lives in the
  `cggmp24` half.

## Concern 2: audit currency

The intuition that a more recent release implies a more current audit does not
survive contact with the evidence.

### `tss-lib` — old audit, but publicly attacked and hardened

- Kudelski Security, **October 2019**, against v1.0.0
  (`audit-binance-tss-lib-final-20191018.pdf`, attached to the v1.0.0 release).
  Stale as a document.
- What matters more is what followed. Verichains' **TSSHOCK** (2023)
  demonstrated key-extraction attacks against TSS implementations including
  tss-lib; **Fireblocks** disclosed separately; **CVE-2022-47931** came out of a
  Kudelski audit of io.Finnet's fork and was fixed upstream in PR #233.
  **v2.0.0** shipped fixes for the Fireblocks and Verichains disclosures with a
  recommendation that all parties update. DeFiOS runs v4.
- Net: a library that has been attacked by multiple independent teams in public
  and hardened in response.

### `cggmp24` — newer audit, but not of the shipped code

- The Kudelski report in the repo is **"Audit of CGGMP21"**, DFNS, June 2023
  engagement, report dated **21 Sep 2023**, scoped to `dfns-labs/cggmp21` plus
  `generic-ec` and `paillier-zk` at pinned commits. Findings: 1 Medium
  (tree-hash commitment malleability, remediated) + 2 Informational. It covers
  both n-out-of-n and t-out-of-n keygen — **of the predecessor codebase**.
- Current code is **0.7.0-alpha**; alpha.3 was published broken.
- The README states **timing attacks are deliberately out of scope** — no
  constant-time operations, for performance.
- Most significant for DeFiOS: **the CGGMP paper covers only n-out-of-n. The
  t-of-n threshold protocols are the authors' own CGGMP24-like design**, not the
  peer-reviewed construction. DeFiOS is t-of-n.
- The README also contradicts itself on key refresh (intro claims it, limitations
  deny it, and a `cggmp24-key-refresh` directory exists). Unresolved.

### `givre` — unaudited signing layer

No public audit. It inherits the audited UC-secure DKG via `cggmp21-keygen`, but
**the FROST signing layer — the exact code that would produce Taproot
signatures — has never been audited.**

### Summary

An old audit on code that has since been attacked and fixed, versus a newer
audit on code that is not the code that would run, plus an unaudited signing
layer on top. "More recent" tracks release dates, not assurance.

## Concern 3: how essential is Taproot?

Published adoption figures conflict badly and should be treated with low
confidence.

- Glassnode's Taproot Adoption metric (share of transactions spending at least
  one Taproot input) read **28.8% in March 2026** but **4.4% in July 2026**. A
  swing that large in four months indicates the metric tracks something other
  than organic payments.
- Third-party summaries claim 15–20%, down from a >40% peak in early 2024,
  without citing datasets.
- The likely explanation: **Taproot transaction counts are dominated by
  inscriptions/Ordinals, not payments.** One analysis found ~99% of Taproot
  transactions since 2024 were dust. The 2024 peak and subsequent collapse track
  inscription activity. For a custody wallet these numbers are near-meaningless.

Figures that do bear on the decision:

- **P2WPKH remains the recommended default in 2026** in most wallet guidance,
  with **~95% receive support across wallets versus ~80% for P2TR**.
- **DeFiOS can already pay Taproot.** Per
  [ADR 0010](../adr/0010-signing-session-semantics.md), outputs may pay any
  address type including P2TR (`signreq.OutputScript`). The gap is **only
  receiving/custodying** at a `bc1p` address, never paying a counterparty who
  wants one.
- Key-path fee savings are modest: ~57 vB input versus ~68 vB for P2WPKH.
- Minor point against P2TR: it reveals its output public key at creation,
  whereas an unspent P2WPKH output stays behind a hash commitment — a small
  hedge in the quantum discussion.

**The strongest argument the other way**, and not an adoption argument: for a
*threshold* wallet, a FROST key-path spend is indistinguishable on-chain from a
single-signature spend, so the multi-party nature of the Key leaks nothing.
P2WPKH already provides this for key-path purposes, so the marginal privacy gain
over DeFiOS's current setup is small; the larger Taproot payoff (script-path
spends, hidden policy trees) is not something DeFiOS needs today. This makes
Taproot a deliberate "later", not a "never" — as ADR 0010 already phrased it.

## Conclusion

All three lines of enquiry point the same way: stay on `tss-lib`, do not package
Taproot now. Greenfield status (no users, keys, funds, or tagged releases)
removes the migration cost from the equation — but it equally removes any
forcing function, and the Lockness stack is a moving target that will be
materially more mature at the next review. Recorded in
[ADR 0011](../adr/0011-threshold-signing-engine-stays-on-tss-lib.md) with
explicit reopening triggers.

## Sources

- [LFDT-Lockness org](https://github.com/LFDT-Lockness)
- [`givre` repository](https://github.com/LFDT-Lockness/givre) and [docs.rs/givre](https://docs.rs/givre/latest/givre/) (ciphersuites: [module docs](https://docs.rs/givre/latest/givre/ciphersuite/index.html))
- [`cggmp21`/`cggmp24` repository](https://github.com/LFDT-Lockness/cggmp21) and [README on the `cggmp24/m` branch](https://github.com/LFDT-Lockness/cggmp21/blob/cggmp24/m/README.md)
- [`cggmp24` on crates.io](https://crates.io/crates/cggmp24), [`givre` on crates.io](https://crates.io/crates/givre)
- [`bnb-chain/tss-lib` releases](https://github.com/bnb-chain/tss-lib/releases) (v1.0.0 audit PDF, v2.0.0 security fixes)
- [Verichains, TSSHOCK](https://verichains.io/tsshock/)
- [io.Finnet, security disclosure for ECDSA and EdDSA threshold signature schemes](https://www.iofinnet.com/post/security-disclosure-for-ecdsa-and-eddsa-threshold-signature-schemes) (CVE-2022-47931)
- [Common MPC Pitfalls](http://mpcsec.org/bugs/)
- [Frank Denis, Performance of WebAssembly runtimes in 2026](https://00f.net/2026/06/23/webassembly-runtimes-2026/) and [the 2023 edition](https://00f.net/2023/01/04/webassembly-benchmark-2023/)
- [Glassnode, BTC Taproot Adoption](https://studio.glassnode.com/charts/transactions.TaprootAdoption?a=BTC)
- [DeFiOS #11](https://github.com/pamacure/defios/issues/11) — prior Lockness evaluation
