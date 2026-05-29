package console

import "embed"

// embeddedFS is the entire console resource tree: templates + static
// assets. The dev-mode override (Config.DevDir) bypasses this in favor
// of disk reads so template edits hot-reload without a rebuild.
//
//go:embed all:layouts all:pages all:partials all:static
var embeddedFS embed.FS
