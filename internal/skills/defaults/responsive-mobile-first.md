---
name: responsive-mobile-first
description: Mobile-first rules every published page must satisfy (single column, viewport, touch targets)
agents: [pages]
---

# Make the page work on every phone

Most visitors open the shared link on a phone, from a messaging app. You
cannot preview the page, so these rules are not advice — they are the
contract. A page that scrolls sideways, squeezes text into half the
screen or needs pinch-zoom is a failed page, whatever it might look like
on a desktop.

## The layout contract

1. **One column. Period.** The base layout is a single centered column
   (`width: min(100% - 2rem, 42rem); margin-inline: auto;`). An image
   NEVER sits beside text: it goes full-width above or below it. If you
   are about to write `grid-template-columns: 1fr 1fr`, a flex row
   holding text and an image, or a float — stop: that is the exact
   mistake this rule exists to prevent.
2. The only multi-column construct allowed is a self-collapsing card
   grid: `grid-template-columns: repeat(auto-fit, minmax(240px, 1fr))`.
   It becomes one column on phones on its own. Media queries are for
   ENHANCING wide screens (widening the measure, adding grid columns),
   never for repairing a broken phone layout after the fact.
3. `<meta name="viewport" content="width=device-width, initial-scale=1">`
   in the `<head>` of every HTML file. Without it nothing else matters.
4. No horizontal scroll, ever: `img, video { max-width: 100%; height:
   auto; }`, no fixed pixel width on any container, `overflow-wrap:
   anywhere` on long URLs. A wide table gets its own `overflow-x: auto`
   wrapper — the body never scrolls sideways.

## Touch and comfort

- Tap targets (buttons, links styled as buttons) at least 44px tall,
  with visible focus styles.
- Base font size 1rem minimum — never shrink text to make it fit; cut
  content instead.
- No `position: fixed` banners eating screen height; no hover-only
  affordances (there is no hover on a phone).
- Navigation on a one-page site is a short vertical list or plain
  anchors — no hamburger menus.

## Media weight

Photos and videos placed with `use_file` are served as stored: tell the
user when a media file is heavy (several MiB), prefer the smaller
variant when one exists, and put `loading="lazy"` on every image below
the fold.

## Final self-check, every time

Before reporting done, re-read your CSS and answer honestly:
- Is there ANY rule putting two things side by side outside the
  auto-fit grid? Remove it.
- Does every HTML file carry the viewport meta?
- Would the page render as one readable column on a 360px screen with
  every `@media` block deleted? It must.
