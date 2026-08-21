---
name: compress-media-for-messaging
description: Shrink a video or photo so it can be sent over a messaging app
agents: [workspace]
---

# Fit a video or photo under the messaging limit

The ceiling is about 15 MB. Above it, the platform rejects the file and the
user gets nothing.

## Measure before deciding

```
ls -l input.mp4
ffprobe -v error -show_entries stream=width,height,duration,codec_name -of json input.mp4
```

A file already under the limit needs no work: say so and attach it as is.

## Video

Start with the resolution, not the bitrate: halving the width divides the
size by roughly four with far less visible damage than crushing quality.

```
ffmpeg -y -i input.mp4 -vf "scale=720:-2" -c:v libx264 -preset veryfast -crf 28 -c:a aac -b:a 96k out.mp4
```

Then check with `ls -l`. Still too big? Raise `-crf` to 32, then scale to
`480:-2`. Two attempts, not ten. A long clip that stays over the limit at
480p should be trimmed instead — ask which part matters before cutting.

`-c:a copy` fails when the source audio codec is unusual; `-c:a aac` is the
safe default.

## Photo

```
convert input.jpg -resize 2000x2000\> -quality 82 out.jpg
```

The `\>` matters: it shrinks larger images and leaves smaller ones alone.
For a scanned document, keep the resolution and lower the quality instead —
text survives compression better than downscaling.

## Deliver

`attach_file`, then say in one line what you changed (new size, new
resolution). The user should know the file they received is not the
original.
