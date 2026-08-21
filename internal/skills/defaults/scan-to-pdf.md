---
name: scan-to-pdf
description: Turn photos of documents into a clean, readable PDF
agents: [workspace]
---

# Photos of a document to a PDF

The usual case: someone photographs a letter, a form or a receipt with a
phone and wants a proper PDF.

## One page at a time

For each photo, straighten and clean it:

```
convert page1.jpg -auto-orient -deskew 40% -trim +repage -colorspace Gray -normalize -quality 85 clean1.jpg
```

- `-auto-orient` applies the phone's rotation flag; without it a page comes
  out sideways.
- `-deskew 40%` straightens a photo taken at a slight angle.
- `-trim +repage` removes the table or desk around the sheet.
- `-colorspace Gray -normalize` lifts grey paper to white and darkens the
  ink. Skip both when the document has meaningful colour — a stamp, a
  highlighted line, a photo ID.

## Assemble

```
convert clean1.jpg clean2.jpg clean3.jpg -page A4 scanned.pdf
```

Keep the order the user gave you. Never reorder pages on your own.

## Make the text searchable

A PDF of photos holds no text: nobody can search it, and neither can you.
Run OCR on it — `-l fra` for French, `-l eng` for English, `-l fra+eng`
when both appear:

```
ocrmypdf --image-dpi 300 -l fra scanned.pdf output.pdf
```

`--image-dpi` is required when the source has no resolution metadata, which
is the norm for a phone photo; without it the command refuses to run. Add
`--force-ocr` when the PDF already carries a text layer you want replaced.

Check what was read before delivering:

```
pdftotext output.pdf - | head -20
```

If the text is garbled, the source photo is too blurred or too skewed — say
so and ask for a better shot rather than delivering an unusable file.

## Check once

```
pdftoppm -png -r 60 -f 1 -l 1 output.pdf preview
```

Then `view_file` on `preview-1.png`: is the page straight, complete and
readable? If a corner is cut off, the `-trim` was too aggressive — redo
that page without it.

## Deliver

`attach_file` on the PDF. Say how many pages it has and that its text is
searchable. If a page was
unreadable at the source — blurred, cropped by the photographer — say which
one rather than delivering it silently.
