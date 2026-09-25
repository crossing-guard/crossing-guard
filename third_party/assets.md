# Shipped console assets

The console resource review covers the embedded static tree in this source candidate.
The two standalone artwork files are `internal/daemon/static/logo.svg` and `favicon.svg`:
repository-maintained SVG checkpoint marks, each constructed from a stroked open ring
and a circle. The selected console-ring design is retained unchanged.

The SVGs contain local geometry and color rules, with no external image, font, script or
linked resource. The favicon uses heavier geometry for a small display. The embedded tree
contains HTML, CSS, JavaScript/tests and these SVGs; no raster artwork, font files, video,
WebAssembly asset or downloaded icon package is distributed there. The inspected styles
contain no remote imports or URL-loaded resources. Font declarations use system fonts.
Inline UI artwork is maintained in the application's JavaScript; the source scan found no
third-party icon-package attribution or import. This is a resource/source provenance
review, not a trademark search or an independent certification of authorship.

Application API/network behavior is separate from static asset loading. A console with no
remote artwork can still communicate through its documented application features.
