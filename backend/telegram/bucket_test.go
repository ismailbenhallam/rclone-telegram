package telegram

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func memAcct(t *testing.T, f *Fs) *memAccount {
	t.Helper()
	return f.r.acct.(*memAccount)
}

func TestParseBucketOp(t *testing.T) {
	op := bucketOp{Op: opBucketMv, Name: "a", ID: 3, To: "b"}
	got, ok := parseBucketOp(op.encode())
	require.True(t, ok)
	assert.Equal(t, op, *got)
	for _, s := range []string{
		"", "hello", `#rclone_bucket {}`, `#rclone_bucket {"op":"add","name":"a"}`,
		`#rclone_bucket {"op":"mv","name":"a","id":1}`, `#rclone_bucket {"op":"x","name":"a","id":1}`,
	} {
		_, ok := parseBucketOp(s)
		assert.False(t, ok, s)
	}
}

// Each bucket is its own channel; the root lists the buckets.
func TestBuckets(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	a := newTestFsAt(t, "bucketsA", "buckets", "")
	put(t, a, "b1/x", "xxx", now)
	put(t, a, "b2/sub/y", "yyy", now)
	require.NoError(t, a.Mkdir(ctx, "empty"))
	assert.Equal(t, []string{"b1", "b2", "empty"}, names(t, a, ""))
	assert.Equal(t, []string{"b2/sub"}, names(t, a, "b2"))
	assert.Equal(t, 3, memAcct(t, a).channelCount())

	// Files need a bucket.
	info := object.NewStaticObjectInfo("loose", now, 1, true, nil, nil)
	_, err := a.Put(ctx, bytes.NewBufferString("x"), info)
	assert.Error(t, err)
	_, err = a.NewObject(ctx, "b1")
	assert.ErrorIs(t, err, fs.ErrorObjectNotFound)
	_, err = a.List(ctx, "missing")
	assert.ErrorIs(t, err, fs.ErrorDirNotFound)

	// Another process finds the buckets through the registry.
	b := newTestFsAt(t, "bucketsB", "buckets", "")
	assert.Equal(t, []string{"b1", "b2", "empty"}, names(t, b, ""))
	assert.Equal(t, "yyy", read(t, b, "b2/sub/y"))

	// An Fs rooted in a bucket sees paths relative to it.
	sub := newTestFsAt(t, "bucketsSub", "buckets", "b2/sub")
	assert.Equal(t, []string{"y"}, names(t, sub, ""))
	_, err = NewFs(ctx, "bucketsSub", "b2/sub/y", sub.r.m)
	assert.ErrorIs(t, err, fs.ErrorIsFile)

	// Server-side copy between buckets; move falls back to copy+delete.
	x, err := a.NewObject(ctx, "b1/x")
	require.NoError(t, err)
	_, err = a.Copy(ctx, x, "b2/x2")
	require.NoError(t, err)
	assert.Equal(t, "xxx", read(t, a, "b2/x2"))
	_, err = a.Move(ctx, x, "b2/x3")
	assert.ErrorIs(t, err, fs.ErrorCantMove)

	// Copy into a new bucket creates it.
	_, err = a.Copy(ctx, x, "b3/x")
	require.NoError(t, err)
	assert.Equal(t, "xxx", read(t, a, "b3/x"))

	// A non-empty bucket can't be removed; an empty one can.
	assert.ErrorIs(t, a.Rmdir(ctx, "b2"), fs.ErrorDirectoryNotEmpty)
	require.NoError(t, a.Rmdir(ctx, "empty"))

	// Purge deletes the whole channel.
	require.NoError(t, a.Purge(ctx, "b2"))
	assert.Equal(t, []string{"b1", "b3"}, names(t, a, ""))
	assert.Equal(t, 2, memAcct(t, a).channelCount())
	assert.ErrorIs(t, a.Purge(ctx, "b2"), fs.ErrorDirNotFound)
	assert.ErrorIs(t, a.Purge(ctx, "b1/sub"), fs.ErrorCantPurge)

	// The other process notices the deletion after a registry sync.
	require.NoError(t, b.r.syncRegistry(ctx))
	_, err = b.List(ctx, "b2")
	assert.ErrorIs(t, err, fs.ErrorDirNotFound)
	assert.Equal(t, []string{"b1", "b3"}, names(t, b, ""))
}

// Renaming a bucket only rewrites the registry.
func TestBucketRename(t *testing.T) {
	ctx := context.Background()
	a := newTestFsAt(t, "renameA", "rename", "")
	put(t, a, "old/f", "data", time.Now())
	put(t, a, "taken/g", "g", time.Now())

	assert.ErrorIs(t, a.DirMove(ctx, a, "old", "taken"), fs.ErrorDirExists)
	require.NoError(t, a.DirMove(ctx, a, "old", "new"))
	assert.Equal(t, []string{"new", "taken"}, names(t, a, ""))
	assert.Equal(t, "data", read(t, a, "new/f"))
	assert.Equal(t, 2, memAcct(t, a).channelCount())

	b := newTestFsAt(t, "renameB", "rename", "")
	assert.Equal(t, []string{"new", "taken"}, names(t, b, ""))
	assert.Equal(t, "data", read(t, b, "new/f"))
}

// Moving a directory to another bucket forwards its files.
func TestDirMoveAcrossBuckets(t *testing.T) {
	ctx := context.Background()
	a := newTestFsAt(t, "acrossA", "across", "")
	put(t, a, "src/dir/a", "aaa", time.Now())
	put(t, a, "src/dir/sub/b", "", time.Now())
	require.NoError(t, a.Mkdir(ctx, "src/dir/empty"))
	put(t, a, "src/keep", "k", time.Now())

	require.NoError(t, a.DirMove(ctx, a, "src/dir", "dst/moved"))
	assert.Equal(t, []string{"src/keep"}, names(t, a, "src"))
	assert.Equal(t, []string{"dst/moved/empty", "dst/moved/sub", "dst/moved/a"}, names(t, a, "dst/moved"))
	assert.Equal(t, "aaa", read(t, a, "dst/moved/a"))
	assert.Equal(t, "", read(t, a, "dst/moved/sub/b"))

	// Moving a whole bucket into another one deletes the source bucket.
	require.NoError(t, a.DirMove(ctx, a, "src", "dst/whole"))
	assert.Equal(t, []string{"dst"}, names(t, a, ""))
	assert.Equal(t, "k", read(t, a, "dst/whole/keep"))
	assert.Equal(t, 1, memAcct(t, a).channelCount())
}

// When two processes create the same bucket, both end up on the first
// channel and the duplicate is deleted.
func TestBucketCreateRace(t *testing.T) {
	ctx := context.Background()
	a := newTestFsAt(t, "raceA", "race", "")
	b := newTestFsAt(t, "raceB", "race", "")
	require.NoError(t, a.Mkdir(ctx, "same"))
	want, ok := a.r.ref("same")
	require.True(t, ok)

	// b has not synced the registry yet and creates the bucket too.
	got, err := b.r.createBucket(ctx, "same")
	require.NoError(t, err)
	assert.Equal(t, want, got)
	assert.Equal(t, 1, memAcct(t, a).channelCount())
}

func TestBucketName(t *testing.T) {
	ctx := context.Background()
	a := newTestFsAt(t, "nameA", "name", "")
	long := string(make([]rune, maxBucketName+1))
	err := a.Mkdir(ctx, long)
	assert.Error(t, err)
	assert.False(t, errors.Is(err, fs.ErrorDirNotFound))
}

// Removing a bucket checks for files uploaded by other processes, even
// before the next refresh.
func TestRmdirBucketStaleIndex(t *testing.T) {
	ctx := context.Background()
	a := newTestFsAt(t, "staleA", "stale", "")
	b := newTestFsAt(t, "staleB", "stale", "")
	require.NoError(t, a.Mkdir(ctx, "bk"))
	assert.Equal(t, []string{}, append([]string{}, names(t, a, "bk")...))
	put(t, b, "bk/new", "data", time.Now())

	assert.ErrorIs(t, a.Rmdir(ctx, "bk"), fs.ErrorDirectoryNotEmpty)
	assert.Equal(t, "data", read(t, b, "bk/new"))

	// Moving the whole bucket carries the other process's file along.
	require.NoError(t, a.DirMove(ctx, a, "bk", "other/bk"))
	assert.Equal(t, "data", read(t, a, "other/bk/new"))
}

// rclone cleanup runs the backend cleanup on every bucket.
func TestCleanUpAllBuckets(t *testing.T) {
	ctx := context.Background()
	a := newTestFsAt(t, "cleanupA", "cleanupall", "")
	put(t, a, "b1/keep", "k", time.Now())
	c := bc(t, newTestFsAt(t, "cleanupA", "cleanupall", "b1"))
	ms := c.st.(*memStore)
	orphan, err := ms.sendFile(ctx, "o", []byte("x"), (&meta{FileID: "o", Path: "o", Last: true, Size: 1}).encode())
	require.NoError(t, err)
	_, err = ms.sendText(ctx, (&meta{Op: opDelete, FileID: "o", Path: "o"}).encode())
	require.NoError(t, err)

	require.NoError(t, a.CleanUp(ctx))
	ms.mu.Lock()
	_, hasOrphan := ms.msgs[orphan]
	ms.mu.Unlock()
	assert.False(t, hasOrphan)
	assert.Equal(t, "k", read(t, a, "b1/keep"))
}
