package telegram

import (
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestFs opens bucket "bkt" of remote `name` on the in-memory account
// `channel`. Different names on one account behave like separate
// processes.
func newTestFs(t *testing.T, name, channel string) *Fs {
	t.Helper()
	return newTestFsAt(t, name, channel, "bkt")
}

// newTestFsAt opens root on remote `name` of the in-memory account
// `channel`.
func newTestFsAt(t *testing.T, name, channel, root string) *Fs {
	t.Helper()
	m := configmap.Simple{
		"api_id":           "1",
		"api_hash":         "x",
		"channel_title":    channel,
		"flush_interval":   "1h", // flush only when a test asks
		"refresh_interval": "0",
	}
	for _, o := range fs.MustFind("telegram").Options {
		if _, ok := m[o.Name]; !ok && o.Default != nil {
			m[o.Name] = o.String()
		}
	}
	f, err := NewFs(context.Background(), name, root, m)
	require.NoError(t, err)
	return f.(*Fs)
}

// bc returns the conn of the root bucket of f, creating the bucket.
func bc(t *testing.T, f *Fs) *conn {
	t.Helper()
	c, err := f.r.conn(context.Background(), f.rootBucket, true)
	require.NoError(t, err)
	return c
}

func put(t *testing.T, f *Fs, remote, content string, mtime time.Time) fs.Object {
	t.Helper()
	src := object.NewStaticObjectInfo(remote, mtime, int64(len(content)), true, nil, nil)
	o, err := f.Put(context.Background(), bytes.NewBufferString(content), src)
	require.NoError(t, err)
	return o
}

func read(t *testing.T, f *Fs, remote string) string {
	t.Helper()
	o, err := f.NewObject(context.Background(), remote)
	require.NoError(t, err)
	r, err := o.Open(context.Background())
	require.NoError(t, err)
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	return string(b)
}

func names(t *testing.T, f *Fs, dir string) []string {
	t.Helper()
	entries, err := f.List(context.Background(), dir)
	require.NoError(t, err)
	var out []string
	for _, e := range entries {
		out = append(out, e.Remote())
	}
	return out
}

// Every change must survive a crash before the index snapshot is saved.
func TestCrashBeforeFlush(t *testing.T) {
	ctx := context.Background()
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	a := newTestFs(t, "crashA", "crash")
	put(t, a, "a.txt", "aaa", mtime)
	b := put(t, a, "b.txt", "bbb", mtime)
	put(t, a, "dir/c.txt", "ccc", mtime)
	require.NoError(t, a.Mkdir(ctx, "empty"))
	require.NoError(t, a.Mkdir(ctx, "gone"))
	require.NoError(t, a.Rmdir(ctx, "gone"))
	_, err := a.Move(ctx, b, "moved.txt")
	require.NoError(t, err)
	require.NoError(t, a.DirMove(ctx, a, "dir", "dir2"))
	o, err := a.NewObject(ctx, "a.txt")
	require.NoError(t, err)
	require.NoError(t, o.Remove(ctx))
	o, err = a.NewObject(ctx, "moved.txt")
	require.NoError(t, err)
	newTime := mtime.Add(time.Hour)
	require.NoError(t, o.SetModTime(ctx, newTime))

	// A "crashes": no flush. A new process replays the channel.
	b2 := newTestFs(t, "crashB", "crash")
	assert.Equal(t, []string{"dir2", "empty", "moved.txt"}, names(t, b2, ""))
	assert.Equal(t, []string{"dir2/c.txt"}, names(t, b2, "dir2"))
	assert.Equal(t, "bbb", read(t, b2, "moved.txt"))
	o, err = b2.NewObject(ctx, "moved.txt")
	require.NoError(t, err)
	assert.True(t, o.ModTime(ctx).Equal(newTime))
}

// Two processes on one channel see each other's changes after a sync.
func TestTwoWriters(t *testing.T) {
	ctx := context.Background()
	a := newTestFs(t, "twoA", "two")
	b := newTestFs(t, "twoB", "two")
	put(t, a, "x", "from a", time.Now())
	put(t, b, "y", "from b", time.Now())
	require.NoError(t, bc(t, a).sync(ctx))
	require.NoError(t, bc(t, b).sync(ctx))
	assert.Equal(t, []string{"x", "y"}, names(t, a, ""))
	assert.Equal(t, []string{"x", "y"}, names(t, b, ""))
	assert.Equal(t, "from b", read(t, a, "y"))

	// Snapshots from both writers merge instead of overwriting each other.
	require.NoError(t, bc(t, a).flush(ctx))
	require.NoError(t, bc(t, b).flush(ctx))
	c := newTestFs(t, "twoC", "two")
	assert.Equal(t, []string{"x", "y"}, names(t, c, ""))
}

// A snapshot is loaded and only newer messages are replayed.
func TestSnapshotReload(t *testing.T) {
	ctx := context.Background()
	a := newTestFs(t, "snapA", "snap")
	put(t, a, "one", "1", time.Now())
	require.NoError(t, bc(t, a).flush(ctx))
	put(t, a, "two", "2", time.Now())

	b := newTestFs(t, "snapB", "snap")
	assert.NotZero(t, bc(t, b).idxMsgID, "snapshot should be found")
	assert.Equal(t, []string{"one", "two"}, names(t, b, ""))
}

// An upload whose parts straddle a snapshot is still indexed.
func TestUploadAcrossSnapshot(t *testing.T) {
	ctx := context.Background()
	a := newTestFs(t, "spanA", "span")
	st := bc(t, a).st
	data := []string{"hello ", "world"}

	file, err := st.upload(ctx, "f", bytes.NewBufferString(data[0]), 6)
	require.NoError(t, err)
	m0 := meta{FileID: "span1", Path: "f", Part: 0, ModTime: 1}
	_, err = st.sendFile(ctx, "f", file, m0.encode())
	require.NoError(t, err)

	bc(t, a).retryLater() // mark dirty
	require.NoError(t, bc(t, a).flush(ctx))

	file, err = st.upload(ctx, "f", bytes.NewBufferString(data[1]), 5)
	require.NoError(t, err)
	m1 := meta{FileID: "span1", Path: "f", Part: 1, ModTime: 1, Last: true, Size: 11}
	_, err = st.sendFile(ctx, "f", file, m1.encode())
	require.NoError(t, err)

	b := newTestFs(t, "spanB", "span")
	assert.Equal(t, "hello world", read(t, b, "f"))
}

// Large files are split into parts and read back across part boundaries.
func TestMultipartRange(t *testing.T) {
	ctx := context.Background()
	f := newTestFs(t, "multi", "multi")
	_, err := f.setUploadChunkSize(4)
	require.NoError(t, err)
	o := put(t, f, "big", "0123456789", time.Now())
	assert.Len(t, o.(*Object).parts, 3)

	r, err := o.Open(ctx, &fs.RangeOption{Start: 3, End: 8})
	require.NoError(t, err)
	b, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.Equal(t, "345678", string(b))
}

// cleanup removes orphaned and abandoned parts but keeps fresh uploads.
func TestCleanup(t *testing.T) {
	ctx := context.Background()
	f := newTestFs(t, "clean", "clean")
	ms := bc(t, f).st.(*memStore)
	put(t, f, "keep", "keep", time.Now())

	send := func(m meta, age time.Duration) int {
		ms.now = func() time.Time { return time.Now().Add(-age) }
		defer func() { ms.now = time.Now }()
		id, err := ms.sendFile(ctx, m.Path, []byte("x"), m.encode())
		require.NoError(t, err)
		return id
	}
	// Orphan: a file whose delete op was logged but whose data remains.
	orphan := send(meta{FileID: "orph", Path: "o", Last: true, Size: 1}, 0)
	_, err := ms.sendText(ctx, (&meta{Op: opDelete, FileID: "orph", Path: "o"}).encode())
	require.NoError(t, err)
	// Abandoned: an unfinished upload from two days ago.
	old := send(meta{FileID: "old", Path: "p"}, 48*time.Hour)
	// In progress: an unfinished upload from a minute ago.
	fresh := send(meta{FileID: "new", Path: "q"}, time.Minute)

	n, err := bc(t, f).cleanup(ctx)
	require.NoError(t, err)
	assert.Equal(t, 2, n)
	ms.mu.Lock()
	_, hasOrphan := ms.msgs[orphan]
	_, hasOld := ms.msgs[old]
	_, hasFresh := ms.msgs[fresh]
	ms.mu.Unlock()
	assert.False(t, hasOrphan)
	assert.False(t, hasOld)
	assert.True(t, hasFresh)
	assert.Equal(t, "keep", read(t, f, "keep"))
}

// rebuild replays the channel from scratch and reaches the same state.
func TestRebuild(t *testing.T) {
	ctx := context.Background()
	f := newTestFs(t, "rebuild", "rebuild")
	put(t, f, "a/b", "b", time.Now())
	require.NoError(t, f.Mkdir(ctx, "empty"))
	before := names(t, f, "")
	n, err := bc(t, f).rebuild(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, before, names(t, f, ""))
}
