# Plugins bring Skills and MCP Servers

Agents need abilities DeFiOS will not build in: indexers, explorers,
audits of contracts. A **Plugin** is a package of **Skills** (instructions)
and **MCP Servers** (programs) installed Node-wide from a git repository or
a local path, under `<data root>/plugins/<plugin>/`; DeFiOS's own come
preinstalled. Nothing in
it reaches an agent until that agent's profile grants the item by name
([ADR 0029](0029-an-agent-profile-holds-the-whole-agent.md)). A Plugin never
adds a Tool: only Tools act as the Node.

## Considered options

- **Plugins as code inside the serve** (Go plugins, scripts it loads).
  Rejected: third-party code in the process that holds the unsealed Vault,
  and Go plugins are brittle across builds. An MCP Server is a separate
  process by design and any language can write one.
- **Granting whole Plugins.** Rejected: a profile names each Skill and MCP
  Server, qualified by its Plugin, so a grant is never wider than intended
  and two Plugins may both have an `audit` Skill.
- **Skills inlined into the system prompt.** Rejected: every Skill would be
  paid for on every call, and the cached prefix
  ([ADR 0018](0018-an-agent-says-one-message.md)) would change with each
  grant. The preamble lists granted Skills by name and description, and the
  agent reads one with `skill_load` when it needs it.
- **Plugin files in a Backup.** Rejected: they can be fetched again, as the
  COSL replica can ([ADR 0027](0027-a-backup-is-the-node-minus-what-peers-give-back.md)).
  The Backup holds the lock file, which is config.

## Consequences

- **Layout.** `plugins/<plugin>/plugin.toml` gives `name`, `version`,
  `description`, and `[mcp.<server>]` tables (`command`, `args`, and the env
  vars it asks for); Skills are `skills/<skill>/SKILL.md` with `name` and
  `description` frontmatter, beside any files they refer to. Plugin, Skill
  and server names follow the Handle rule.
- **Install.** `plugin install <source>` takes a git URL or a local path; it
  shows the commands the Plugin's MCP Servers run and asks first. `plugin
  list`, `plugin update` and `plugin remove` complete the set; removing a
  Plugin a profile still grants is refused, naming the profile.
- **Built-in Plugins.** DeFiOS's own Plugins live in this repository under
  `plugins/<plugin>/`, in the same layout, and are compiled into the binary
  and read from it, never written to `<data root>/plugins/`. A built-in
  Plugin is always the binary's version: it needs no install, no prompt and
  no lock entry, and cannot be edited, updated or removed. Its MCP Servers
  run the `defios` binary itself (`defios mcp <plugin>/<server>`), so there
  is nothing else to ship. An installed Plugin may not take a built-in's
  name. Built-ins are granted per agent by name like any other Plugin.
- **No Registries yet.** Installing by name from an index, DeFiOS's or a
  community's, is a later decision; until then a source is a URL or a path.
- **Lock file.** `<config root>/plugins.lock` records each Plugin's source
  and pinned commit or hash, so a Backup's restore reinstalls exactly the
  same code. A Plugin installed from a local path cannot be fetched again; a
  restore reports it missing and goes on.
- **MCP Server processes.** The serve starts a server the first time an agent
  granting it is summoned, shares it between every agent that grants it, and
  stops it with the serve or a switch of Current Group. Stdio only. It starts
  with a clean environment: `PATH`, `HOME`, and only the Secrets the granting
  profile names, never the serve's own environment, which may hold every
  Secret.
- **Names to the model.** An MCP Server's tools are offered as
  `<plugin>__<server>__<tool>`, since providers allow only
  `[a-zA-Z0-9_-]{1,64}`; a longer name is an error when the agent is
  summoned. Only tools are used; MCP resources and prompts are not.
- **Transcripts stay plaintext.** They now hold Tool and MCP results; those
  came from the plaintext replica or from a program the person granted.
- **Later.** Registries, signed Plugins, and remote (HTTP) MCP Servers.
