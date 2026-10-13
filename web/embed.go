// Package web embeds the static front end so one Go binary serves both the
// UI and the API.
package web

import "embed"

//go:embed index.html style.css app.js config.js
var FS embed.FS
