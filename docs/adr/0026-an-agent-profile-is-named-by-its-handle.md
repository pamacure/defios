# An Agent Profile is named by its Handle

[ADR 0015](0015-agent-profiles-handles-and-agent-chains.md) let a Group's
`[agents.<handle>]` table bind its Handle to any profile directory through
`dir`, so two Handles could share one prompt and one Handle could run a
different prompt in each Group. Nothing has used that, and it makes the
directory a second identity beside the Handle: a CLI that creates, edits or
deletes a profile would have to ask which Groups bind which directory, and
guard against a `dir` that points anywhere on disk. An Agent Profile's
directory is now always `<config root>/agents/<handle>/`, and `dir` is gone.
This amends ADR 0015 and
[ADR 0019](0019-providers-replace-models-on-the-cosl.md).

## Considered options

- **Keep `dir`, defaulting to `agents/<handle>`** (ADR 0015). Rejected: the
  sharing it allows is not needed, and every verb that touches a profile
  directory would carry the cost of it, from finding who else binds it to
  refusing to delete a path outside the config root.
- **Name the directory by Handle and Group** (`agents/<group>/<handle>`).
  Rejected: an operator who wants the same `@auditor` in two Groups would
  keep two copies of its prompt, which is what ADR 0015 set out to avoid.

## Consequences

- **One Handle, one prompt on a Node.** The same Handle in two Groups
  reads the same `SYSTEM.md`; its Provider, body and `max_turns` are still
  each Group's own. Two Handles cannot share a prompt, and a Handle cannot
  have a different prompt per Group: either needs another Handle and its own
  directory.
- **`dir` is an unknown key.** A config that sets `[agents.<handle>] dir` is
  refused when it is read, as any unknown key is. No deployed config is
  known to set it, so nothing is migrated.
- **A profile directory is shared exactly when its Handle is.** Whether
  another Group uses a profile is whether that Group's config defines the
  same Handle.
