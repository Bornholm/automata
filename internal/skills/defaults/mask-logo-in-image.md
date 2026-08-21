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

## 3. Attach first, then verify

Call `attach_file` on the result **before** checking it. Delivering is the
only step the user actually sees; a file you never hand over is worth
nothing, however good it looks.

Then, once — and only once — call `view_file` on the result: "is the logo
still visible?". If the mask is clearly off, adjust the region, rerun the
command and attach the corrected version. Never more than one such round.

Never sample textures to reconstruct the background, never probe pixels one
by one, never verify more than once: that is how a task that takes two
commands ends up producing nothing at all.

The result is a blurred patch where the logo was, not an invisible repair.
That is the expected outcome — say so plainly instead of chasing a perfect
reconstruction that these tools cannot produce.
