# Raft reaches a Roster Node by its identity, not its tailnet name

Raft's StreamLayer (:4632) and forward gate (:4633) dialled
`<TsnetHostname>:<port>`, the name each Roster Node signed into its Identity
at formation, and MagicDNS resolved it. But headscale, not the Node, owns a
machine's name. A Node restored from a Backup is a new machine on the
tailnet, because a Backup leaves the tsnet state behind
([ADR 0027](0027-a-backup-is-the-node-minus-what-peers-give-back.md)).
While the original's registration still stands, headscale gives the
restored machine another name (`defios-0007a620-homenet-1`), and every other
Node's Raft dials the original, which is offline. Discovery still finds the
restored Node, because it fetches each peer's Identity by tailnet IP, so the
header showed every device online while nothing could commit. In a Group of
two that's a deadlock: the restored Node's empty log loses every vote it
asks for, and the other Node's vote requests never reach it.

Raft now reaches a Roster Node at the tailnet IP of the online peer whose
Identity verifies as that Node, with the same keys the Roster pinned, as
discovery already checks. Raft still names Nodes by `TsnetHostname`, so
Raft's stored configuration doesn't change. serve gives `raftx` a Transport
that turns the name into a verified IP: it uses the IP it last verified,
asks every online peer who it is (at most every three seconds) when it has
none or a dial there fails, and dials the name only when no peer verifies.

## Considered options

- **Keep dialling the name, and tell the operator to rename the machine
  in headscale after a restore.** Rejected: an easy step to miss, a manual
  step in every restore, and nothing in the app says why messages queue.
- **Raft addresses by Node ID or IP.** Rejected: the addresses live in
  Raft's stored configuration, and an IP changes with a re-registration
  too. Keeping the name as the key changes nothing stored.
- **Re-sign the Roster with the new name.** Rejected: the Roster is the
  Group's fixed, signed membership. A name the control plane hands out
  doesn't belong in it.
- **Trust the peer whose MagicDNS name starts with the pinned name.**
  Rejected: a name says nothing about who answers. An Identity verified
  against the Roster's keys does.

## Consequences

- Backing up, wiping the Node directories and restoring needs no headscale
  step for Raft. The restored Node registers with the Group's auth key
  Secret, if that key is still valid, and the others reach it under
  whatever name headscale gave it. Removing the original's registration
  is still good housekeeping. The restore note keeps saying so.
- While a Roster Node can't be reached, its peers fetch every online peer's
  Identity at most every three seconds, a request per peer on a tailnet of
  a few machines.
- serve logs once when it reaches a Node under a name other than the
  Roster's, so the rename is visible.
- Discovery's "online" and Raft's reach now agree on who a peer is. Whether
  the header's majority should also require a Raft leader is a separate
  question.
