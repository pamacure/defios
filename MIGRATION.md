# Migration

DeFiOS is pre-release and keeps no backwards compatibility: a renamed file or
setting is no longer read under its old name. Each section below says what
changed and how to bring an existing Node up to date. Run them with the
Node's serve stopped.

The commands use the Node's config root: `--dir` if you pass one, else
`$DEFIOS_CONFIG_DIR`, else `$XDG_CONFIG_HOME/defios`, else
`~/.config/defios`.

```bash
CONFIG="${DEFIOS_CONFIG_DIR:-${XDG_CONFIG_HOME:-$HOME/.config}/defios}"
```

## `SYSTEM.md` is now `BRIEF.md`, and `say` is now `talk_write` (ADR 0036)

An Agent Profile's instructions are its **Agent Brief**, `BRIEF.md`.
`SYSTEM.md` is no longer read, so a profile with only `SYSTEM.md` fails
when it is summoned, saying it has no Agent Brief. Rename each one:

```bash
for f in "$CONFIG"/agents/*/SYSTEM.md; do
  [ -e "$f" ] || continue
  if [ -e "${f%/*}/BRIEF.md" ]; then
    echo "skipped $f: ${f%/*}/BRIEF.md already exists" >&2
  else
    mv "$f" "${f%/*}/BRIEF.md"
  fi
done
```

The Tool an agent posts to talk with is now `talk_write`. A profile with no
`tools` line gets it by default, as it got `say`. A profile that lists its
Tools and names `say` no longer loads until `say` is replaced:

```bash
for f in "$CONFIG"/agents/*/config.toml; do
  [ -e "$f" ] || continue
  sed -i.bak -E '/^[[:space:]]*tools[[:space:]]*=/ s/"say"/"talk_write"/g' "$f" && rm "$f.bak"
done
```

Check the result with `defios agent show <handle>` for each profile.
