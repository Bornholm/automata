---
name: mask-logo-in-image
description: Remove or mask a logo or watermark on a photo with imagemagick
agents: [workspace]
---

# Mask a logo or watermark on an image

Follow these four steps. Do not invent your own method: this one works and
fits in the budget.

## 1. Locate the logo

Call `view_file` on the image and ask for the position and size of the logo
in pixels: "where is the logo, and how big is it? give x, y, width, height".
Do not extract crops, do not dump pixels, do not compute histograms.

## 2. Mask it in ONE command

Pad the reported box by about 10% on each side, then blur that region:

```
convert in.jpg -region WxH+X+Y -blur 0x12 out.jpg
```

On a flat background — a plain sky, a uniform wall — a median filter blends
better than a blur:

```
convert in.jpg -region WxH+X+Y -statistic median 15x15 out.jpg
```

If the logo sits in a corner and cropping is acceptable, cropping it away is
also a legitimate answer — say so when you do it.

## 3. Verify once

One `view_file` on the result: "is the logo still visible?". Once. If the
mask is slightly off, adjust the region and rerun the command a single time.

## 4. Attach

Call `attach_file` on the result. State briefly what you did.

Never sample textures to reconstruct the background, never probe pixels one
by one, never verify more than once: that is how a task that takes two
commands ends up producing nothing at all.
