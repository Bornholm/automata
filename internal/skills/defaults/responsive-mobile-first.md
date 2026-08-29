---
name: responsive-mobile-first
description: Mobile-first rules every published page must satisfy (viewport, breakpoints, touch targets)
agents: [pages]
---

# Make the page work on every phone

Most visitors will open the shared link on a phone, from a messaging app.
A page that scrolls sideways or needs pinch-zoom is a failed page,
whatever it looks like on desktop.

## Non-negotiables

- `<meta name="viewport" content="width=device-width, initial-scale=1">`
  in the `<head>` of every HTML file. Without it nothing else matters.
- The page must be fully usable at 360 px wide **with no media query at
  all**: single column, fluid widths, wrapping text. That is the base
  layout you write first.
- No horizontal scroll, ever: `img, video { max-width: 100%; height: auto; }`,
  no fixed pixel width on any container, `word-break` on long URLs. A
  wide table or code block gets its own `overflow-x: auto` wrapper — the
  body never scrolls sideways.

## Then enhance upward

- At most two breakpoints, written as `@media (min-width: 40rem)` and
  `@media (min-width: 60rem)`, used to add columns or widen the measure —
  never to fix a broken mobile layout after the fact.
- Grids collapse on their own with
  `grid-template-columns: repeat(auto-fit, minmax(240px, 1fr))` — prefer
  that to breakpoint-managed columns.
- Navigation on mobile is a short vertical list or plain anchors. Do not
  build hamburger menus for a one-page site.

## Touch and comfort

- Tap targets (links styled as buttons, actual buttons) at least 44 px
  tall, with visible focus styles.
- Base font size 1rem minimum — never shrink text to make it fit; cut
  content instead.
- No `position: fixed` banners eating screen height; no hover-only
  affordances (there is no hover on a phone).

## Media weight

Photos and videos placed with `use_file` are served as stored: mention to
the user when a media file is heavy (several MiB), and prefer the smaller
variant when one exists. `loading="lazy"` on every image below the fold.
