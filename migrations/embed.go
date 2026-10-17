package migrations

import "embed"

// Files contains the versioned PostgreSQL migration pairs.
//
//go:embed *.up.sql *.down.sql
var Files embed.FS
