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
// html2canvas.min.js is html2canvas 1.4.1 (MIT, Niklas von Hertzen), copied
// unmodified from the npm tarball's dist/ (registry sha512
// fPU6BHNpsyIhr8yyMpTLLxAbkaK8ArIBcmZIRiBLiDhjeqvXolaEmDGmELFuX9I4xDcaKKcJl+TKZLqruBbmWA==).
// The viewer lazy-loads it to capture gallery thumbnails.
//
//go:embed ui.css ui.js html2canvas.min.js
var UI embed.FS
