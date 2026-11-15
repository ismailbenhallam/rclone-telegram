package selfupdate

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAssetName(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, goarm, want string }{
		{"linux", "amd64", "", "rclone-telegram-v1.76.1-linux-amd64.tar.gz"},
		{"linux", "arm64", "", "rclone-telegram-v1.76.1-linux-arm64.tar.gz"},
		{"linux", "arm", "7", "rclone-telegram-v1.76.1-linux-armv7.tar.gz"},
		{"darwin", "arm64", "", "rclone-telegram-v1.76.1-darwin-arm64.tar.gz"},
		{"freebsd", "amd64", "", "rclone-telegram-v1.76.1-freebsd-amd64.tar.gz"},
		{"windows", "amd64", "", "rclone-telegram-v1.76.1-windows-amd64.zip"},
	} {
		assert.Equal(t, tc.want, assetName("v1.76.1", tc.goos, tc.goarch, tc.goarm))
	}
}

func TestParseVersion(t *testing.T) {
	v, ok := parseVersion("v1.76.12")
	require.True(t, ok)
	assert.Equal(t, [3]int{1, 76, 12}, v)
	for _, bad := range []string{"", "dev", "1.76.1", "v1.76", "v1.76.1-3-gabc123", "v1.76.1-dirty", "v1.76.1-beta"} {
		_, ok := parseVersion(bad)
		assert.False(t, ok, bad)
	}
}

func TestNewer(t *testing.T) {
	v := func(s string) [3]int { p, ok := parseVersion(s); require.True(t, ok); return p }
	assert.True(t, newer(v("v1.76.1"), v("v1.76.0")))
	assert.True(t, newer(v("v1.77.0"), v("v1.76.9")))
	assert.True(t, newer(v("v1.76.10"), v("v1.76.9")))
	assert.False(t, newer(v("v1.76.0"), v("v1.76.0")))
	assert.False(t, newer(v("v1.76.0"), v("v1.76.1")))
}

func sumLine(name string, data []byte) string {
	s := sha256.Sum256(data)
	return hex.EncodeToString(s[:]) + "  " + name + "\n"
}

func TestVerify(t *testing.T) {
	archive := []byte("archive bytes")
	const name = "rclone-telegram-v1.76.1-linux-arm64.tar.gz"
	sums := sumLine("other.zip", []byte("x")) + sumLine(name, archive)

	assert.NoError(t, verify(name, archive, []byte(sums)))
	// sha256sum binary mode marker
	assert.NoError(t, verify(name, archive, []byte(strings.Replace(sums, "  "+name, " *"+name, 1))))
	// Windows line endings
	assert.NoError(t, verify(name, archive, []byte(strings.ReplaceAll(sums, "\n", "\r\n"))))

	err := verify(name, []byte("tampered"), []byte(sums))
	assert.ErrorContains(t, err, "checksum verification failed")
	err = verify(name, archive, []byte(sumLine("other.zip", archive)))
	assert.ErrorContains(t, err, "not listed")
	err = verify(name, archive, nil)
	assert.ErrorContains(t, err, "not listed")
	err = verify(name, archive, []byte("garbage\n"))
	assert.ErrorContains(t, err, "malformed")
}

func tarGz(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range files {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gz.Close())
	return buf.Bytes()
}

func zipFile(t *testing.T, files map[string]string) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, body := range files {
		w, err := zw.Create(name)
		require.NoError(t, err)
		_, err = w.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, zw.Close())
	return buf.Bytes()
}

func TestExtract(t *testing.T) {
	dir := "rclone-telegram-v1.76.1-linux-arm64/"
	tgz := tarGz(t, map[string]string{dir + "README.md": "readme", dir + "rclone-telegram": "BIN"})
	bin, err := extract("x.tar.gz", tgz, "rclone-telegram")
	require.NoError(t, err)
	assert.Equal(t, "BIN", string(bin))

	z := zipFile(t, map[string]string{dir + "LICENSE": "l", dir + "rclone-telegram.exe": "EXE"})
	bin, err = extract("x.zip", z, "rclone-telegram.exe")
	require.NoError(t, err)
	assert.Equal(t, "EXE", string(bin))

	_, err = extract("x.tar.gz", tarGz(t, map[string]string{dir + "README.md": "r"}), "rclone-telegram")
	assert.ErrorContains(t, err, "not found")
	_, err = extract("x.tar.gz", []byte("<html>Not Found</html>"), "rclone-telegram")
	assert.Error(t, err)
	_, err = extract("x.tar.gz", tarGz(t, map[string]string{"rclone-telegram": ""}), "rclone-telegram")
	assert.ErrorContains(t, err, "empty")
	_, err = extract("x.7z", tgz, "rclone-telegram")
	assert.ErrorContains(t, err, "unsupported")
}

// fakeReleases serves /latest and /download/<tag>/<asset> like GitHub.
func fakeReleases(t *testing.T, latest string, assets map[string][]byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/releases/latest":
			if latest == "" {
				http.Redirect(w, r, "/releases", http.StatusFound)
				return
			}
			http.Redirect(w, r, "/releases/tag/"+latest, http.StatusFound)
		case strings.HasPrefix(r.URL.Path, "/releases/download/"):
			data, ok := assets[strings.TrimPrefix(r.URL.Path, "/releases/download/")]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(data)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	old := releasesURL
	releasesURL = srv.URL + "/releases"
	t.Cleanup(func() { releasesURL = old })
}

func TestLatestTag(t *testing.T) {
	fakeReleases(t, "v1.76.3", nil)
	tag, err := latestTag(context.Background(), http.DefaultClient)
	require.NoError(t, err)
	assert.Equal(t, "v1.76.3", tag)

	fakeReleases(t, "", nil)
	_, err = latestTag(context.Background(), http.DefaultClient)
	assert.ErrorContains(t, err, "no published release")

	fakeReleases(t, "nightly", nil)
	_, err = latestTag(context.Background(), http.DefaultClient)
	assert.ErrorContains(t, err, "not a version")
}

func TestDownload(t *testing.T) {
	const tag = "v1.76.1"
	asset := assetName(tag, "linux", "amd64", "")
	// extract looks for the host's executable name.
	archive := tarGz(t, map[string]string{"d/" + exeName(binaryName, runtime.GOOS): "NEWBIN"})
	ctx := context.Background()

	fakeReleases(t, tag, map[string][]byte{
		tag + "/" + asset:    archive,
		tag + "/" + sumsFile: []byte(sumLine(asset, archive)),
	})
	bin, err := download(ctx, http.DefaultClient, tag, asset)
	require.NoError(t, err)
	assert.Equal(t, "NEWBIN", string(bin))

	// Tampered archive: refused.
	fakeReleases(t, tag, map[string][]byte{
		tag + "/" + asset:    []byte("tampered"),
		tag + "/" + sumsFile: []byte(sumLine(asset, archive)),
	})
	_, err = download(ctx, http.DefaultClient, tag, asset)
	assert.ErrorContains(t, err, "checksum verification failed")
	assert.ErrorContains(t, err, "nothing installed")

	// Missing checksum file: refused.
	fakeReleases(t, tag, map[string][]byte{tag + "/" + asset: archive})
	_, err = download(ctx, http.DefaultClient, tag, asset)
	assert.ErrorContains(t, err, "refusing to install an unverified binary")

	// Missing asset.
	fakeReleases(t, tag, map[string][]byte{tag + "/" + sumsFile: []byte(sumLine(asset, archive))})
	_, err = download(ctx, http.DefaultClient, tag, asset)
	assert.ErrorContains(t, err, "404")
}

func TestRunCheck(t *testing.T) {
	fakeReleases(t, "v1.76.3", nil)
	defer func(v string) { Version = v }(Version)
	ctx := context.Background()

	Version = "v1.76.3"
	assert.NoError(t, Run(ctx, &Options{Check: true}))
	Version = "v1.76.1"
	assert.NoError(t, Run(ctx, &Options{Check: true}))
	Version = "dev"
	assert.ErrorContains(t, Run(ctx, &Options{Check: true}), "not a release build")
	assert.ErrorContains(t, Run(ctx, &Options{Version: "latest"}), "expected a release tag")
}

func TestInstallOutput(t *testing.T) {
	out := filepath.Join(t.TempDir(), "rclone-telegram")
	require.NoError(t, install([]byte("NEWBIN"), out))
	got, err := os.ReadFile(out)
	require.NoError(t, err)
	assert.Equal(t, "NEWBIN", string(got))
}
