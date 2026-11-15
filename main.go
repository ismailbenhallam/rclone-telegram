// Command rclone-telegram is a full rclone binary with an extra "telegram"
// backend compiled in.
package main

import (
	"runtime/debug"

	_ "github.com/ismailbenhallam/rclone-telegram/backend/telegram" // telegram backend
	"github.com/ismailbenhallam/rclone-telegram/cmd/selfupdate"     // replaces upstream's selfupdate
	_ "github.com/rclone/rclone/backend/all"                        // all upstream backends
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/all" // all commands
	"github.com/rclone/rclone/fs"
	_ "github.com/rclone/rclone/lib/plugin" // plugins
)

// version is set at release time with -ldflags "-X main.version=vX.Y.Z".
var version = ""

func main() {
	v := version
	if v == "" {
		// `go install ...@vX.Y.Z` records the module version.
		if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		} else {
			v = "dev"
		}
	}
	fs.Version = fs.VersionTag + "-telegram-" + v
	selfupdate.Version = v
	cmd.Main()
}
