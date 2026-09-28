// Package web embeds the frontend so the service ships as one binary.
package web

import "embed"

//go:embed files
var Assets embed.FS
