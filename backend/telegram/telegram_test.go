package telegram

import (
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fstest"
	"github.com/rclone/rclone/fstest/fstests"
)

// TestMain runs every test against the in-memory channel, unless a real
// remote is given with -remote (e.g. go test -remote TestTelegram:).
func TestMain(m *testing.M) {
	flag.Parse()
	if *fstest.RemoteName == "" {
		newAccount = fakeAccount
	}
	// fstests looks for rclone's test server scripts relative to the
	// working directory. This backend needs none: give it an empty dir.
	tmp, err := os.MkdirTemp("", "rclone-telegram-test")
	if err == nil {
		err = os.MkdirAll(filepath.Join(tmp, "fstest", "testserver", "init.d"), 0o755)
	}
	if err == nil {
		err = os.Chdir(tmp)
	}
	if err != nil {
		panic(err)
	}
	code := m.Run()
	_ = os.RemoveAll(tmp)
	os.Exit(code)
}

// TestIntegration runs rclone's generic backend test suite.
func TestIntegration(t *testing.T) {
	fstests.Run(t, &fstests.Opt{
		RemoteName: ":telegram,api_id=1,api_hash=x,channel_title=integration:",
		NilObject:  (*Object)(nil),
		ChunkedUpload: fstests.ChunkedUploadConfig{
			MinChunkSize: 64 * fs.Kibi,
			MaxChunkSize: 2 * maxChunk,
		},
		QuickTestOK: true,
	})
}

func (f *Fs) SetUploadChunkSize(cs fs.SizeSuffix) (fs.SizeSuffix, error) {
	return f.setUploadChunkSize(cs)
}
