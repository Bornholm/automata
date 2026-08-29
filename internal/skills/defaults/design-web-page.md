---
name: design-web-page
description: Visual design system for building a polished web page by hand (tokens, type, color, dark mode)
agents: [pages]
---

# Design a polished web page

Load this before writing any HTML or CSS. You have no framework, no build
step and no network at authoring time: everything below is achievable with
one hand-written `style.css`, and that constraint is the style — clean,
airy, typographic.

## Structure

- Semantic HTML5: `header`, `main`, `section`, `footer`, real headings in
  order (`h1` once, then `h2`…). Never build layout out of bare `div`s
  when an element says what the block is.
- One stylesheet, `style.css`, linked with a relative path. No inline
  `style=` attributes, no CSS framework, no base64 media, no text baked
  into images.

## Design tokens first

Open `style.css` by defining the whole system as custom properties on
`:root`, then use ONLY the tokens below in the rest of the file:

```css
:root {
  --bg: #faf9f7;         /* page ground, slightly warm, never pure white */
  --surface: #ffffff;    /* cards */
  --text: #1c1c1e;       /* near-black, never #000 */
  --muted: #6b6b70;
  --accent: #0f6b54;     /* ONE accent hue; pick it to match the subject */
  --radius: 12px;
  --space-1: 0.5rem; --space-2: 1rem; --space-3: 2rem; --space-4: 4rem;
  --shadow: 0 1px 3px rgb(0 0 0 / 0.08), 0 8px 24px rgb(0 0 0 / 0.06);
}
@media (prefers-color-scheme: dark) {
  :root { --bg:#141416; --surface:#1e1e21; --text:#f2f2f2;
          --muted:#9a9aa1; --accent:#4cc9a4;
          --shadow: 0 1px 3px rgb(0 0 0 / 0.5); }
}
```

Dark mode costs those few lines when every color goes through a token —
always include it. Keep text/background contrast at AA (4.5:1); check the
accent on both grounds.

## Typography carries the design

- Fluid sizes with `clamp()`: `h1 { font-size: clamp(1.8rem, 4vw + 1rem, 3rem); }`,
  body at `1rem/1.6`. Two or three sizes total — restraint reads as intent.
- Reading measure: `max-width: 65ch` on text blocks, centered with margin
  auto and `padding-inline: var(--space-2)`.
- Font: the system stack (`system-ui, sans-serif`) is a fine default. One
  Google Fonts family is allowed for headings (the page is public, the
  visitor's browser fetches it): a single `<link>`, `font-display: swap`,
  and a real fallback in the stack.

## Composition

- Space is the main design tool: section padding from the scale
  (`var(--space-3)` up), consistent gaps, no crammed blocks.
- Layout with flex and grid only; never fixed pixel widths on containers.
  A card grid: `display:grid; grid-template-columns:repeat(auto-fit,minmax(240px,1fr)); gap:var(--space-2);`.
- Media: `img, video { max-width:100%; height:auto; border-radius:var(--radius); display:block; }`
  and `object-fit: cover` on fixed-ratio thumbnails.
- Finish: one consistent `--radius`, the soft `--shadow` on cards,
  `transition: 150ms` on links/buttons hover. Nothing blinking, nothing
  auto-playing with sound.

## Judge it before publishing

Re-read the HTML as a visitor: is there one clear focal point, is the
hierarchy obvious from size and space alone, does every color come from a
token? If a section looks busy, remove decoration rather than adding more.
