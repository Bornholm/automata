---
name: strip-photo-metadata
description: Read or erase the hidden metadata of a photo before sharing it
agents: [workspace]
---

# Photo metadata

A photo taken with a phone carries more than the image: GPS coordinates,
the exact date and time, the device model, sometimes the owner's name.
Sharing the file shares all of it.

## See what is in there

```
exiftool -G -s photo.jpg
```

Report the revealing fields in plain words — location, date, device — not
the raw dump. Say clearly if the file holds GPS coordinates: that is the
one most people do not expect.

## Erase everything

```
exiftool -all= -overwrite_original photo.jpg
```

The image itself is untouched; only the metadata goes. Do this whenever the
user asks to share a photo publicly, or before publishing it anywhere.

## Erase the location only

When the date matters — a document photographed as proof, a dated record —
keep it and drop the position alone:

```
exiftool -gps:all= -overwrite_original photo.jpg
```

## Verify and deliver

Re-run `exiftool -G -s` on the result and confirm what is gone. Then
`attach_file`, and state in one line what you removed. Never claim a photo
is clean without having checked the file you actually produced.
