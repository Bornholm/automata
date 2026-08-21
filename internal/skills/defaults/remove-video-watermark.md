---
name: remove-video-watermark
description: Remove a logo or watermark from a video with ffmpeg delogo
agents: [workspace]
---

# Remove a watermark from a video

## 1. Probe the file

```
ffprobe -v error -show_streams -show_format -of json input.mp4
```

Read the real width, height and duration from it. Never guess them.

## 2. Locate the watermark

Extract ONE frame, then look at it:

```
ffmpeg -y -i input.mp4 -ss 1 -frames:v 1 frame.png
```

Then `view_file` on `frame.png`: "where is the watermark, and how big is it?
give x, y, width, height". Scale the coordinates if the frame you looked at
is not at the video's native resolution.

## 3. Remove it in ONE pass

`delogo` is the first choice: it interpolates from the surrounding pixels
and keeps the whole frame.

```
ffmpeg -y -i input.mp4 -vf "delogo=x=X:y=Y:w=W:h=H" \
  -c:v libx264 -preset veryfast -crf 28 -c:a copy output.mp4
```

If the watermark touches an edge and losing that strip is acceptable, crop
instead — and tell the user you cropped:

```
ffmpeg -y -i input.mp4 -vf "crop=W:H:X:Y" -c:v libx264 -preset veryfast -crf 28 -c:a copy output.mp4
```

## 4. Check the size

```
ls -l output.mp4
```

Messaging platforms reject anything much over 15 MB. If it is larger,
re-encode harder in one go: raise `-crf` to 30-32 and scale down with
`-vf "scale=-2:720"`.

## 5. Attach

Call `attach_file` on the result and say briefly what you did.
