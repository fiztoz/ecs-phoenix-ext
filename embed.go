// Package ecsphoenixext exposes embedded assets owned by the ecs-phoenix-ext extension.
package ecsphoenixext

import "embed"

// Migrations holds this repo's SQL, applied by the plugin on start.
// Phoenix never owns these tables.
//
//go:embed migrations/*.sql
var Migrations embed.FS
