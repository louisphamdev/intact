// Package web carries the dashboard inside the binary, so deployment stays one
// file and no build step is needed at run time.
package web

import "embed"

//go:embed index.html icons/*.png
var Files embed.FS
