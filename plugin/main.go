// Command plugin builds the telegram backend as an rclone Go plugin.
//
//	go build -buildmode=plugin -o librcloneplugin_telegram.so ./plugin
//
// rclone loads it from $RCLONE_PLUGIN_PATH. The plugin only loads into an
// rclone binary built with cgo, the same Go version and the exact same
// versions of every shared dependency. Official rclone releases do not
// meet that, so most users should use the standalone binary instead.
package main

import (
	_ "github.com/ismailbenhallam/rclone-telegram/backend/telegram" // registers the backend
)

func main() {}
