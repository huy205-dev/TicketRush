// Package migrations embeds the goose migrations so tests can apply them
// without the goose CLI. `make migrate` still uses the CLI on these files.
package migrations

import "embed"

// FS holds every migration file.
//
//go:embed *.sql
var FS embed.FS
