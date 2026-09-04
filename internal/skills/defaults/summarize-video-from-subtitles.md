---
name: summarize-video-from-subtitles
description: Summarize or search what a video says, from its subtitles, without downloading the video
agents: [workspace]
---

# Summarize a video from its subtitles

When someone asks what a video says — summarise it, find a passage, quote
it — you do not need the video. Its subtitles are a few tens of kilobytes
of text and arrive in seconds, where the video would take minutes and
hundreds of megabytes.

Never call `download_video` for this.

## 1. Fetch the subtitles

```
download_subtitles {"url": "https://www.youtube.com/watch?v=...", "name": "talk"}
```

Pass `lang` only when you know the video is spoken in another language:
`{"lang": "de,en"}`. The tool reports which language it actually got.

If it answers that no subtitles exist, stop. Say so plainly. Do not try
other forms of the same link, and do not download the video instead —
there is no transcription tool here.

## 2. Clean it before reading it

**Never `cat` the `.vtt` file.** Automatic subtitles repeat every line two
or three times and carry a timing tag around every word: an hour of video
is 300 to 500 KB raw, which floods your context and tells you nothing
extra.

Turn it into plain text first:

```
sed -e 's/<[^>]*>//g' -e 's/&nbsp;/ /g' talk.vtt \
  | grep -v -e '-->' -e '^WEBVTT' -e '^Kind:' -e '^Language:' -e '^NOTE' \
  | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \
  | awk 'NF && $0 != prev { print; prev = $0 }' > talk.txt
wc -c talk.txt
```

Use this variant instead when the user wants to be pointed at a moment of
the video — it keeps a `[hh:mm:ss]` marker every five minutes:

```
awk '
BEGIN { lastb = -1 }
/-->/ { split($1, t, "."); ts = t[1]; next }
      { gsub(/<[^>]*>/, ""); gsub(/&nbsp;/, " ")
        sub(/^[ \t]+/, ""); sub(/[ \t]+$/, "") }
/^WEBVTT|^Kind:|^Language:|^NOTE/ { next }
NF == 0 || $0 == prev { next }
      { split(ts, p, ":"); b = int((p[1] * 60 + p[2]) / 5)
        if (b != lastb) { print "[" ts "]"; lastb = b }
        print; prev = $0 }
' talk.vtt > talk.txt
```

## 3. Read it

```
cat talk.txt
```

Above roughly 100 KB, read it in slices rather than at once — `sed -n
'1,800p' talk.txt`, then `sed -n '801,1600p'`, and so on. Summarise each
slice as you go instead of holding the whole transcript.

## 4. Answer

Write the summary in the language of the user's question, not the language
of the video.

Two things automatic subtitles do NOT contain, and that you must never
invent:

- **who speaks.** There are no speaker labels. In an interview or a panel,
  write "one speaker argues that…", never a name.
- **punctuation.** Sentence boundaries are your reading, not the source's.
  Because of that, never present a line as a verbatim quotation unless the
  user asked for one, and say it is approximate when you do.

Say the summary comes from the subtitles. If they were automatic, they
contain transcription errors — proper nouns and technical terms are the
first to suffer — and it costs one sentence to say so.
