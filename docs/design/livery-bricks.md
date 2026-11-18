# Livery bricks: design brief

A **Livery** (see `GLOSSARY.md`) is the colour and brick this Node marks a
party of talk with: one of its own agents, another device, or another device's
agent. There are 8 colours and 8 bricks, for 64 Liveries. This brief asks for
the 8 bricks.

## Where a brick appears

- **Avatar**: a square 28px tile filled with the party's colour, the brick
  knocked out of it in the page background. Shown to the left of a message in
  the Talk transcript, bottom-aligned, as in a group chat.
- **Mention chip**: the brick at about 14px, drawn in the party's colour
  beside `@handle` on a faint wash of that colour.
- **Lists**: the `@` autocomplete, the Agents page, the Talk side panel and
  the Livery picker, at about 16px.

Every brick has to read at 14px and stay clearly different from the other
seven when all eight share one colour. The shape alone must tell them apart,
since a colour-blind reader may not tell two colours apart.

## The look to match

The app looks like bricks that snap together (`desktop/frontend/style.css`,
"The look"): a solid lower edge instead of a soft shadow, flat fills, small
radii (2–4px), and Quantico for titles.

The closest existing piece is the **Sign Request meter brick** (`.seg`): one
brick per device needed to sign, 24×12 with a 2px lower edge and a single stud
on top. Make that brick one of the eight, redrawn to the grid below, and draw
the other seven in the same spirit: simple building bricks seen side-on or from
above, for example studs in different counts or arrangements, a slope, an arch,
a round brick, a plate, a tile, a corner.

## Constraints

- **Grid**: 16×16 viewBox, with shapes snapped to whole pixels.
- **One colour**: every shape filled with `currentColor`, so CSS colours it
  from the theme and one file serves all 8 colours in light and dark themes.
  For depth, at most one further path at a lower `fill-opacity` (such as the
  lower edge or the studs). No strokes, gradients, filters or text.
- **Silhouette first**: each brick must still be recognisable as a solid
  knockout inside a filled square.
- **Optical size**: about 1px of padding inside the 16×16 box, so the eight
  look equally big beside each other.
- **Names**: give each brick a short lowercase name with no hyphen or space,
  such as `stud`, `arch` or `slope`. The name is how a Livery is written
  (`blue-arch`), so it cannot change once released.

## Deliverables

Eight files, `desktop/frontend/bricks/<name>.svg`, each a bare `<svg
viewBox="0 0 16 16">` with no width, height or fill set except `currentColor`.
Also include a contact sheet showing all eight at 14px, at 28px as knockouts
in a solid tile, in one colour and in the light and dark themes.
