// Package web holds the browser frontend: plain HTML, CSS and JavaScript
// with no build step, embedded into booking, which serves it (SPEC.md 3.2).
package web

import "embed"

// Files are index.html, app.js and style.css.
//
//go:embed index.html app.js style.css
var Files embed.FS
