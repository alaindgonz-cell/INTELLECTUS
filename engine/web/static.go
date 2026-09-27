package web

import "embed"

// Static holds the single-page operator UI (index.html, app.js, style.css)
// under the "static/" prefix. It is plain HTML/CSS/JS with no build step and
// no external network requests; serve it with fs.Sub(Static, "static").
//
//go:embed static
var Static embed.FS
