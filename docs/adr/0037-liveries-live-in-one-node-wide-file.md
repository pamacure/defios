# Liveries live in one Node-wide file

A Livery is the colour and brick this Node marks a party of talk with: one of
its own Agent Profiles, another Node, or another Node's Agent. Every Livery the
Node has given is kept in one file, `liveries.toml`, beside `agents/` in the
Node's config directory. Entries are keyed by party: `agent:<handle>`,
`node:<node id>` and `node:<node id>/<handle>`. A Livery only changes how talk
looks, but it is not one of the desktop app's machine preferences (ADR 0034).
It belongs to the Node, so a Backup brings the Node back with every party
looking the same, and every front end reads one value.

No two parties may share a Livery. One file makes that a check against one
place, and since a Node ID is the same in every Group, a device keeps its
Livery across the Groups it shares with this Node. Writes refuse a Livery
another party holds. Reads tolerate a duplicate, so a hand edit or a restore
can never stop anything from working. `internal/node` gives a random free
Livery to each party the first time it sees that party, whichever front end
hosts the Node. Deleting an Agent Profile removes its entry, and removing a
Group removes the entries of devices, and their agents, that are in no Group
this Node still has. With only 64 Liveries, old entries would otherwise use
them up.

The Node has no verb that removes a Group, so the second rule is applied
lazily: each time `internal/node` loads the Rosters it holds, it frees the
entries of devices in none of them. A Group removed by hand is caught the
same way. If a Record fails to load, nothing is freed, since that Group's
Roster cannot be read.

## Considered options

- **Desktop prefs keyed by party**: rejected. A restored Node would come back
  unmarked, and no other front end could show the Liveries.
- **A `livery` field in each profile's `config.toml`, plus a file for
  everyone else**: rejected. Uniqueness would have to be checked across two
  stores.
- **Other parties' Liveries in each Group's `config.toml`**: rejected. A
  device would look different in each Group, and uniqueness would span every
  Group's file.
