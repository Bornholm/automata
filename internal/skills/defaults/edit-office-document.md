---
name: edit-office-document
description: Read, edit and convert docx, odt and pdf documents with pandoc and LibreOffice
agents: [workspace]
---

# Work on an office document

## Read it first

Never guess what a document contains — convert it and read it:

```
pandoc report.docx -t markdown
pdftotext report.pdf -
```

## Edit in markdown, convert once

Do the editing in markdown, then convert a single time at the end:

```
pandoc draft.md -o report.docx
office-convert pdf report.docx
```

`office-convert` is the LibreOffice entry point and takes the target format
as its FIRST argument.

## Round trips lose layout

Editing a document the user sent means converting it to markdown, changing
the text, and converting back. A richly formatted document — columns,
styles, headers, embedded images — will not survive that round trip. Say so
plainly before or when you deliver it, rather than silently degrading it.
When only a small change is needed and the layout matters, offer to return
the change as a separate note instead.

## Check a PDF before sending it

Render a page and look at it:

```
pdftoppm -png -r 60 -f 1 -l 1 report.pdf page
```

Then `view_file` on `page-1.png`. One check is enough.

## Finish

Call `attach_file` on the document you produced.
