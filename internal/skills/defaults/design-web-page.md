---
name: design-web-page
description: Visual design system for building a polished web page by hand (skeleton, tokens, type, dark mode)
agents: [pages]
---

# Design a polished web page

You cannot see the page you are writing, so do not invent layout: START
FROM THE SKELETON BELOW, verbatim, and only then adapt the content. Every
part of it is deliberate and mobile-safe. You have no framework, no build
step and no network at authoring time — one hand-written `style.css`, and
that constraint is the style: clean, airy, typographic.

## The skeleton (copy it, then adapt)

`index.html`:

```html
<!doctype html>
<html lang="fr">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>TITLE</title>
<link rel="stylesheet" href="style.css">
</head>
<body>
<header class="hero">
  <!-- The hero image is optional; when present it sits ABOVE the text,
       full width — never beside it. -->
  <img class="hero-img" src="photo.jpg" alt="" loading="eager">
  <p class="kicker">KICKER LINE</p>
  <h1>TITLE</h1>
  <p class="lead">SUBTITLE · DATE</p>
</header>
<main>
  <section>
    <h2>SECTION TITLE</h2>
    <dl class="facts">
      <div><dt>LABEL</dt><dd>VALUE</dd></div>
      <div><dt>LABEL</dt><dd>VALUE</dd></div>
    </dl>
  </section>
  <section>
    <h2>SECTION TITLE</h2>
    <p>BODY TEXT.</p>
  </section>
</main>
<footer><p>FOOTER LINE</p></footer>
</body>
</html>
```

`style.css` (the tokens ARE the design system — use only them below):

```css
:root {
  --bg: #faf9f7;       /* slightly warm ground, never pure white */
  --surface: #ffffff;
  --text: #1c1c1e;     /* near-black, never #000 */
  --muted: #6b6b70;
  --accent: #0f6b54;   /* ONE accent hue; pick it to match the subject */
  --radius: 14px;
  --space-1: .5rem; --space-2: 1rem; --space-3: 2rem; --space-4: 4rem;
  --shadow: 0 1px 3px rgb(0 0 0 / .08), 0 8px 24px rgb(0 0 0 / .06);
}
@media (prefers-color-scheme: dark) {
  :root { --bg:#141416; --surface:#1e1e21; --text:#f2f2f2;
          --muted:#9a9aa1; --accent:#4cc9a4;
          --shadow: 0 1px 3px rgb(0 0 0 / .5); }
}
* { box-sizing: border-box; }
body {
  margin: 0; background: var(--bg); color: var(--text);
  font: 1rem/1.6 system-ui, sans-serif;
}
img, video { max-width: 100%; height: auto; display: block; }
.hero, main, footer {
  width: min(100% - 2 * var(--space-2), 42rem);
  margin-inline: auto;
}
.hero { padding-block: var(--space-3); text-align: center; }
.hero-img {
  width: 100%; max-height: 60vh; object-fit: cover;
  border-radius: var(--radius); box-shadow: var(--shadow);
  margin-bottom: var(--space-3);
}
.kicker { letter-spacing: .18em; text-transform: uppercase;
  font-size: .8rem; color: var(--muted); }
h1 { font-size: clamp(2rem, 5vw + 1rem, 3.2rem); line-height: 1.15;
  margin: var(--space-1) 0; }
.lead { color: var(--accent); font-size: 1.15rem; }
h2 { font-size: 1.35rem; margin: var(--space-3) 0 var(--space-1); }
section { background: var(--surface); border-radius: var(--radius);
  box-shadow: var(--shadow); padding: var(--space-2) var(--space-3);
  margin-block: var(--space-2); }
.facts div { padding-block: var(--space-1);
  border-bottom: 1px dashed color-mix(in srgb, var(--muted) 35%, transparent); }
.facts div:last-child { border-bottom: 0; }
.facts dt { letter-spacing: .14em; text-transform: uppercase;
  font-size: .75rem; color: var(--muted); }
.facts dd { margin: 0; font-size: 1.1rem; }
footer { padding-block: var(--space-3); color: var(--muted);
  text-align: center; font-size: .9rem; }
```

This skeleton is a SINGLE COLUMN at every width, and that is correct: a
42rem centered column reads well on a phone and on a desktop. Do not add
side-by-side layouts. The only allowed multi-column construct is a card
grid that collapses by itself:

```css
.grid { display: grid; gap: var(--space-2);
  grid-template-columns: repeat(auto-fit, minmax(240px, 1fr)); }
```

Never `grid-template-columns: 1fr 1fr`, never floats, never a flex row
holding text next to an image — on a phone half the screen per column is
a failed page.

## Editing files without corrupting them

- To MODIFY an existing file, rewrite it whole (`write_file` without
  `append`). `append: true` exists for ONE case: building a long file
  across consecutive `write_file` calls, where the first part is left
  deliberately unfinished. Appending to a complete document puts content
  after `</html>` — the browser hoists it into the body and wrecks the
  layout.
- Media (`use_file` results) are referenced from INSIDE `<body>` — in the
  hero or a section — never bolted onto the end of the file.

## Adapting without breaking it

- Keep every color a token. Dark mode then costs nothing — keep the dark
  block, adjust only the accent.
- Two or three font sizes total; restraint reads as intent. One Google
  Fonts family is allowed for headings (single `<link>`,
  `font-display: swap`, real fallback in the stack).
- More photos: give each its own full-width `<img>` inside a section, or
  the `.grid` above with `object-fit: cover` thumbnails.
- Finish: one `--radius` everywhere, the soft `--shadow` on cards,
  `transition: 150ms` on links and buttons. Nothing blinking, nothing
  auto-playing.
- Contrast at AA (4.5:1) — check the accent on both grounds.

## Judge it before finishing

Re-read the HTML as a phone visitor: one clear focal point, hierarchy
from size and space alone, every color a token, no element wider than its
container. If a section looks busy, remove decoration rather than adding
more.
