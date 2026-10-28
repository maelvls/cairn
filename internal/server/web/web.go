// Package web holds the embedded browser-facing assets: HTML templates for
// the login page, gallery, viewer and admin UI, their shared stylesheet and
// helpers (ui.css, ui.js), plus the cairn.js client library. Everything is
// self-contained — no CDNs, no build step.
package web

import "embed"

//go:embed *.html
var Templates embed.FS

//go:embed cairn.js
var CairnJS []byte

// UI holds the assets Cairn's own pages share, served under /_ui/.
//
//go:embed ui.css ui.js
var UI embed.FS
