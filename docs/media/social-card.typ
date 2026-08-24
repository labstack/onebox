// The GitHub social preview card, which the documentation site also serves as
// its og:image at /social-card.png.
//
// Render it with `just social-card`, which pins the typst version, requires
// IBM Plex Mono on a font path, and fails when the face is missing. Running
// typst by hand does not: it warns about an unknown font family, exits 0, and
// writes a card set in whatever it found instead.
//
// The page is 960pt x 480pt, which is exactly 1280 x 640 pixels at 96 ppi: the
// size GitHub expects for a social preview, and large enough that a link unfurl
// does not resample it. `site-build` asserts the committed PNG still matches
// those numbers.
//
// The mark is read from the site's favicon rather than copied here, so a
// revised mark reaches the card the next time it is rendered.
#set page(width: 960pt, height: 480pt, margin: (x: 60pt, top: 44pt, bottom: 34pt), fill: rgb("#0d100d"))
#set text(font: "IBM Plex Mono", fill: rgb("#e2ece0"))

#place(top + left, dx: -60pt, dy: -44pt, rect(width: 960pt, height: 5pt, fill: rgb("#4f9a3c")))

#grid(
  columns: (72pt, 1fr),
  column-gutter: 22pt,
  align: horizon,
  image("../../site/public/favicon.svg", width: 68pt),
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
