// Package selfupdate replaces rclone's selfupdate command with one that
// installs rclone-telegram releases from GitHub instead of official rclone.
package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/creativeprojects/go-selfupdate/update"
	"github.com/rclone/rclone/cmd"
	_ "github.com/rclone/rclone/cmd/selfupdate" // registers upstream's command first, so init can remove it
	"github.com/rclone/rclone/fs/config/flags"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/spf13/cobra"
)

const (
	// binaryName is the executable's name inside every release archive.
	binaryName = "rclone-telegram"
	// sumsFile is the checksum asset published with every release.
	sumsFile = "SHA256SUMS.txt"
	// maxDownload caps any single download.
	maxDownload = 512 << 20
)

// releasesURL is the GitHub releases page; a variable so tests can swap it.
var releasesURL = "https://github.com/ismailbenhallam/rclone-telegram/releases"

// Version is the running build's rclone-telegram version, set by main.
var Version string

// Options configure the selfupdate command.
type Options struct {
	Check   bool
	Output  string
	Version string
}

var opt Options

var cmdSelfUpdate = &cobra.Command{
	Use:     "selfupdate",
	Aliases: []string{"self-update"},
	Short:   "Update rclone-telegram to the latest release from GitHub.",
	Long: `Downloads the latest rclone-telegram release for this OS and
architecture from ` + releasesURL + `
and replaces the running binary with it.

It does not install official rclone, which lacks the telegram backend.

The download is checked against the release's ` + sumsFile + `. If the
checksum file is missing, does not list the archive, or the hash does not
match, nothing is installed and the current binary is left untouched.

Use --check to only report whether an update is available.

Use --version vX.Y.Z to install a specific release, older ones included.
Builds that are not a tagged release (dev builds, or ones made from a
commit after a tag) only update when --version is given.

Use --output to write the new binary to another path instead of replacing
the running one. Replacing a binary in a system directory such as
/usr/local/bin needs write access to that directory (for example sudo).`,
	Run: func(command *cobra.Command, args []string) {
		cmd.CheckArgs(0, 0, command, args)
		cmd.Run(false, false, command, func() error {
			return Run(context.Background(), &opt)
		})
	},
}

func init() {
	for _, c := range cmd.Root.Commands() {
		if c.Name() == "selfupdate" {
			cmd.Root.RemoveCommand(c)
		}
	}
	cmd.Root.AddCommand(cmdSelfUpdate)
	f := cmdSelfUpdate.Flags()
	flags.BoolVarP(f, &opt.Check, "check", "", false, "Check for an update without installing it", "")
	flags.StringVarP(f, &opt.Output, "output", "", "", "Write the new binary to this path instead of replacing the running one", "")
	flags.StringVarP(f, &opt.Version, "version", "", "", "Install this release (e.g. v1.76.1) instead of the latest", "")
}

// Run checks for, downloads, verifies and installs a release.
func Run(ctx context.Context, opt *Options) error {
	if opt.Version != "" {
		if _, ok := parseVersion(opt.Version); !ok {
			return fmt.Errorf("--version %q: expected a release tag like v1.76.1", opt.Version)
		}
	}
	client := fshttp.NewClient(ctx)

	target := opt.Version
	if target == "" {
		latest, err := latestTag(ctx, client)
		if err != nil {
			return fmt.Errorf("finding the latest release: %w", err)
		}
		target = latest
	}
	asset := assetName(target, runtime.GOOS, runtime.GOARCH, goarm())

	fmt.Printf("current: %s\n", Version)
	if opt.Version != "" {
		fmt.Printf("requested: %s\n", target)
	} else {
		fmt.Printf("latest: %s\n", target)
		cur, ok := parseVersion(Version)
		if !ok {
			return fmt.Errorf("%q is not a release build: use --version %s to install that release", Version, target)
		}
		lat, _ := parseVersion(target)
		if !newer(lat, cur) {
			fmt.Println("Already up to date.")
			return nil
		}
		if opt.Check {
			fmt.Println("An update is available. Run without --check to install it.")
			return nil
		}
	}
	if opt.Check {
		return nil
	}
	if opt.Version == Version && opt.Output == "" {
		fmt.Println("Already running that version.")
		return nil
	}

	bin, err := download(ctx, client, target, asset)
	if err != nil {
		return err
	}
	return install(bin, opt.Output)
}

// latestTag reads the latest release's tag from the redirect GitHub sends
// for /releases/latest, which needs no API call and has no rate limit.
func latestTag(ctx context.Context, client *http.Client) (string, error) {
	noRedirect := *client
	noRedirect.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, releasesURL+"/latest", nil)
	if err != nil {
		return "", err
	}
	resp, err := noRedirect.Do(req)
	if err != nil {
		return "", err
	}
	_ = resp.Body.Close()
	loc := resp.Header.Get("Location")
	if resp.StatusCode < 300 || resp.StatusCode >= 400 || loc == "" {
		return "", fmt.Errorf("unexpected response %s from %s/latest", resp.Status, releasesURL)
	}
	tag := path.Base(loc)
	if !strings.Contains(loc, "/releases/tag/") {
		return "", fmt.Errorf("no published release (redirected to %s)", loc)
	}
	if _, ok := parseVersion(tag); !ok {
		return "", fmt.Errorf("latest release tag %q is not a version", tag)
	}
	return tag, nil
}

// download fetches the release archive and its checksum file, verifies the
// archive and returns the extracted binary. Any failure aborts the update.
func download(ctx context.Context, client *http.Client, tag, asset string) ([]byte, error) {
	base := releasesURL + "/download/" + tag + "/"
	fmt.Printf("Downloading %s\n", base+asset)
	archive, err := fetch(ctx, client, base+asset)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", asset, err)
	}
	sums, err := fetch(ctx, client, base+sumsFile)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w (refusing to install an unverified binary)", sumsFile, err)
	}
	if err := verify(asset, archive, sums); err != nil {
		return nil, fmt.Errorf("%w: nothing installed", err)
	}
	fmt.Printf("Checksum verified against %s\n", sumsFile)

	bin, err := extract(asset, archive, exeName(binaryName, runtime.GOOS))
	if err != nil {
		return nil, fmt.Errorf("extracting %s: %w", asset, err)
	}
	return bin, nil
}

func fetch(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxDownload+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDownload {
		return nil, fmt.Errorf("larger than %d bytes", maxDownload)
	}
	return data, nil
}

// verify checks archive against its line in sums, a sha256sum-format file.
// A missing entry, a malformed line or a mismatch is an error.
func verify(asset string, archive, sums []byte) error {
	want := ""
	sc := bufio.NewScanner(bytes.NewReader(sums))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		hash, name, ok := strings.Cut(line, " ")
		if !ok {
			return fmt.Errorf("malformed %s line %q", sumsFile, line)
		}
		name = strings.TrimPrefix(strings.TrimLeft(name, " "), "*")
		if name == asset {
			want = strings.ToLower(hash)
			break
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("reading %s: %w", sumsFile, err)
	}
	if want == "" {
		return fmt.Errorf("checksum verification failed: %s is not listed in %s", asset, sumsFile)
	}
	sum := sha256.Sum256(archive)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum verification failed for %s: expected %s, got %s", asset, want, got)
	}
	return nil
}

// extract returns the file called exe from a .tar.gz or .zip archive.
func extract(asset string, archive []byte, exe string) ([]byte, error) {
	var (
		bin []byte
		err error
	)
	switch {
	case strings.HasSuffix(asset, ".zip"):
		bin, err = extractZip(archive, exe)
	case strings.HasSuffix(asset, ".tar.gz"):
		bin, err = extractTarGz(archive, exe)
	default:
		return nil, fmt.Errorf("unsupported archive format")
	}
	if err != nil {
		return nil, err
	}
	if len(bin) == 0 {
		return nil, fmt.Errorf("%s is empty", exe)
	}
	return bin, nil
}

func extractTarGz(archive []byte, exe string) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, err
	}
	t := tar.NewReader(gz)
	for {
		h, err := t.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%s not found in archive", exe)
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == exe {
			return io.ReadAll(io.LimitReader(t, maxDownload))
		}
	}
}

func extractZip(archive []byte, exe string) ([]byte, error) {
	z, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, err
	}
	for _, f := range z.File {
		if f.FileInfo().Mode().IsRegular() && path.Base(f.Name) == exe {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer func() { _ = rc.Close() }()
			return io.ReadAll(io.LimitReader(rc, maxDownload))
		}
	}
	return nil, fmt.Errorf("%s not found in archive", exe)
}

// install writes bin to output, or replaces the running executable when
// output is empty.
func install(bin []byte, output string) error {
	if output != "" {
		if err := os.WriteFile(output, bin, 0o755); err != nil {
			return fmt.Errorf("writing %s: %w", output, err)
		}
		fmt.Printf("Wrote %s\n", output)
		return nil
	}

	target, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the running binary: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(target); err == nil {
		target = resolved
	}
	// Apply writes the new file next to the target, then swaps it in by
	// renaming, which also works for a running binary on Windows.
	if err := update.Apply(bytes.NewReader(bin), update.Options{TargetPath: target}); err != nil {
		if rerr := update.RollbackError(err); rerr != nil {
			return fmt.Errorf("replacing %s failed and so did restoring it: %w; restore it manually", target, rerr)
		}
		if errors.Is(err, os.ErrPermission) {
			return fmt.Errorf("replacing %s: %w (needs write access to its directory, e.g. sudo, or use --output)", target, err)
		}
		return fmt.Errorf("replacing %s: %w", target, err)
	}
	fmt.Printf("Updated %s\n", target)
	return nil
}

// assetName is the release asset for a platform, as built by release.yml.
func assetName(tag, goos, goarch, goarm string) string {
	arch := goarch
	if goarch == "arm" && goarm != "" {
		arch += "v" + goarm
	}
	ext := ".tar.gz"
	if goos == "windows" {
		ext = ".zip"
	}
	return binaryName + "-" + tag + "-" + goos + "-" + arch + ext
}

// goarm returns the GOARM this binary was built with, or "".
func goarm() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, s := range info.Settings {
			if s.Key == "GOARM" {
				// Values may carry a float suffix, e.g. "7,softfloat".
				v, _, _ := strings.Cut(s.Value, ",")
				return v
			}
		}
	}
	return ""
}

func exeName(name, goos string) string {
	if goos == "windows" {
		return name + ".exe"
	}
	return name
}

// releaseVersion matches a tagged release such as v1.76.1.
var releaseVersion = regexp.MustCompile(`^v(\d+)\.(\d+)\.(\d+)$`)

// parseVersion parses a release tag. Anything else, such as a dev build or
// a git describe string, is not a release.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	m := releaseVersion.FindStringSubmatch(v)
	if m == nil {
		return out, false
	}
	for i := range out {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return out, false
		}
		out[i] = n
	}
	return out, true
}

// newer reports whether a is a later version than b.
func newer(a, b [3]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}
