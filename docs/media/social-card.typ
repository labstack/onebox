// The GitHub social preview card, which the documentation site also serves as
// its og:image at /social-card.png.
//
// Render it with IBM Plex Mono on a font path. The face is not installed
// system-wide — the site loads it through @fontsource — so without the path
// the card silently falls back to a proportional face and stops looking like
// the product:
//
//   typst compile --font-path <dir-with-IBMPlexMono> --ppi 96 --format png \
//     docs/media/social-card.typ site/public/social-card.png
//
// The page is 960pt x 480pt, which is exactly 1280 x 640 pixels at 96 ppi: the
// size GitHub expects for a social preview, and large enough that a link unfurl
// does not resample it.
#set page(width: 960pt, height: 480pt, margin: (x: 60pt, top: 44pt, bottom: 34pt), fill: rgb("#0d100d"))
#set text(font: "IBM Plex Mono", fill: rgb("#e2ece0"))

#place(top + left, dx: -60pt, dy: -44pt, rect(width: 960pt, height: 5pt, fill: rgb("#4f9a3c")))

#grid(
  columns: (72pt, 1fr),
  column-gutter: 22pt,
  align: horizon,
  image("social-card-mark.svg", width: 68pt),
  text(size: 56pt, weight: 600, "Onebox"),
)

#v(34pt)
#text(size: 34pt, weight: 600, fill: rgb("#8fd67a"))[
  Plan-before-apply deploys. \
  Zero downtime. One box.
]

#v(18pt)
#text(size: 20pt, fill: rgb("#87957f"))[
  Production operations for one application \
  intentionally running on one Linux server.
]

#place(bottom + left, dy: 0pt, block(width: 840pt)[
  #line(length: 100%, stroke: 0.75pt + rgb("#22291f"))
  #v(12pt)
  #grid(columns: (1fr, auto),
    text(size: 19pt, fill: rgb("#87957f"))[#text(fill: rgb("#4f9a3c"))[\$ ob plan] #h(18pt) sealed diff, then deploy],
    text(size: 19pt, fill: rgb("#87957f"))[onebox.run],
  )
])
