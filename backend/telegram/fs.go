package telegram

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/bucket"
)

// Fs represents a path in the remote. The first path segment is the
// bucket; each bucket is its own channel.
type Fs struct {
	name          string
	root          string
	rootBucket    string // bucket part of root, standard encoding
	rootDirectory string // path in the bucket, standard encoding
	opt           Options
	features      *fs.Features
	r             *remote
}

// NewFs constructs an Fs from the path.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if err := checkChunkSize(opt.ChunkSize); err != nil {
		return nil, err
	}
	if opt.UploadConcurrency < 1 {
		opt.UploadConcurrency = 1
	}
	r, err := getRemote(ctx, name, opt, m)
	if err != nil {
		return nil, err
	}
	f := &Fs{name: name, opt: *opt, r: r}
	f.setRoot(root)
	f.features = (&fs.Features{
		BucketBased:             true,
		BucketBasedRootOK:       true,
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	if f.rootDirectory != "" {
		bkt, full := f.split("")
		c, err := r.conn(ctx, bkt, false)
		if err != nil && !errors.Is(err, errNoBucket) {
			return nil, err
		}
		isFile := false
		if c != nil {
			c.read(func(x *index) { _, isFile = x.Files[full] })
		}
		if isFile {
			f.setRoot(path.Dir(f.root))
			return f, fs.ErrorIsFile
		}
	}
	return f, nil
}

func (f *Fs) setRoot(root string) {
	f.root = cleanPath(root)
	f.rootBucket, f.rootDirectory = bucket.Split(f.root)
}

func checkChunkSize(cs fs.SizeSuffix) error {
	if cs <= 0 || cs > 2*maxChunk {
		return fmt.Errorf("telegram: invalid chunk_size %v (max %v)", cs, 2*maxChunk)
	}
	return nil
}

func (f *Fs) setUploadChunkSize(cs fs.SizeSuffix) (old fs.SizeSuffix, err error) {
	if err = checkChunkSize(cs); err == nil {
		old, f.opt.ChunkSize = f.opt.ChunkSize, cs
	}
	return old, err
}

func cleanPath(p string) string {
	return strings.Trim(path.Clean("/"+p), "/")
}

// split maps a remote path to its encoded bucket name and its encoded
// path in that bucket's index. Both are empty at the root.
func (f *Fs) split(remote string) (bucketName, full string) {
	b, p := bucket.Split(cleanPath(path.Join(f.root, remote)))
	if b == "" {
		return "", ""
	}
	return f.opt.Enc.FromStandardName(b), f.opt.Enc.FromStandardPath(p)
}

// conn returns the conn of a bucket, mapping a missing bucket to
// notFound. With create, a missing bucket is created instead.
func (f *Fs) conn(ctx context.Context, bkt string, create bool, notFound error) (*conn, error) {
	c, err := f.r.conn(ctx, bkt, create)
	if errors.Is(err, errNoBucket) {
		return nil, notFound
	}
	return c, err
}

// Name of the remote.
func (f *Fs) Name() string { return f.name }

// Root of the remote.
func (f *Fs) Root() string { return f.root }

// String converts this Fs to a string.
func (f *Fs) String() string { return fmt.Sprintf("telegram root '%s'", f.root) }

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features { return f.features }

// Precision of modification times.
func (f *Fs) Precision() time.Duration { return time.Nanosecond }

// Hashes returns the supported hash types.
func (f *Fs) Hashes() hash.Set { return hash.Set(hash.MD5) }

// List the objects and directories in dir. At the root, it lists the
// buckets.
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	bkt, full := f.split(dir)
	if bkt == "" {
		for _, name := range f.r.listBuckets(ctx) {
			entries = append(entries, fs.NewDir(f.opt.Enc.ToStandardName(name), time.Time{}))
		}
		return entries, nil
	}
	c, err := f.conn(ctx, bkt, false, fs.ErrorDirNotFound)
	if err != nil {
		return nil, err
	}
	c.maybeSync(ctx)
	child := func(p string) string {
		return path.Join(dir, path.Base(f.opt.Enc.ToStandardPath(p)))
	}
	c.read(func(x *index) {
		if !x.dirExists(full) {
			err = fs.ErrorDirNotFound
			return
		}
		files, dirs := x.list(full)
		entries = make(fs.DirEntries, 0, len(files)+len(dirs))
		for _, d := range dirs {
			entries = append(entries, fs.NewDir(child(d), time.Time{}))
		}
		for _, p := range files {
			entries = append(entries, f.newObject(child(p), x.Files[p]))
		}
	})
	return entries, err
}

func (f *Fs) newObject(remote string, e *fileEntry) *Object {
	return &Object{
		fs:      f,
		remote:  remote,
		size:    e.Size,
		modTime: time.Unix(0, e.ModTime),
		md5:     e.MD5,
		parts:   append([]int(nil), e.Parts...),
		fileID:  e.FileID,
	}
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (o fs.Object, err error) {
	bkt, full := f.split(remote)
	if full == "" {
		return nil, fs.ErrorObjectNotFound
	}
	c, err := f.conn(ctx, bkt, false, fs.ErrorObjectNotFound)
	if err != nil {
		return nil, err
	}
	c.maybeSync(ctx)
	err = fs.ErrorObjectNotFound
	c.read(func(x *index) {
		if e, ok := x.Files[full]; ok {
			o, err = f.newObject(remote, e), nil
		}
	})
	return o, err
}

// Put uploads in to remote.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	o := &Object{fs: f, remote: src.Remote()}
	return o, o.Update(ctx, in, src, options...)
}

// PutStream uploads in to remote when the size is unknown.
func (f *Fs) PutStream(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.Put(ctx, in, src, options...)
}

// Mkdir creates the directory, and its bucket if needed.
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	bkt, full := f.split(dir)
	if bkt == "" {
		return nil
	}
	c, err := f.conn(ctx, bkt, true, nil)
	if err != nil {
		return err
	}
	return c.mkdir(ctx, full)
}

// Rmdir removes an empty directory. At a bucket root, it deletes the
// bucket and its channel.
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	bkt, full := f.split(dir)
	if bkt == "" {
		return nil
	}
	c, err := f.conn(ctx, bkt, false, fs.ErrorDirNotFound)
	if err != nil {
		return err
	}
	if full == "" {
		// Deleting the channel is final: check emptiness against the
		// latest state, including uploads from other processes.
		if err := c.sync(ctx); err != nil {
			return err
		}
	}
	c.read(func(x *index) {
		switch {
		case !x.dirExists(full):
			err = fs.ErrorDirNotFound
		case x.hasChildren(full):
			err = fs.ErrorDirectoryNotEmpty
		}
	})
	if err != nil {
		return err
	}
	if full == "" {
		return f.r.deleteBucket(ctx, bkt)
	}
	return c.logOp(ctx, &meta{Op: opRmdir, Path: full}, func(x *index) {
		delete(x.Dirs, full)
	})
}

// Purge deletes a whole bucket at once by deleting its channel. Other
// directories fall back to deleting file by file.
func (f *Fs) Purge(ctx context.Context, dir string) error {
	bkt, full := f.split(dir)
	if bkt == "" || full != "" {
		return fs.ErrorCantPurge
	}
	return f.r.deleteBucket(ctx, bkt)
}

// Move src to remote server-side. Only the index changes. Moves between
// buckets fall back to Copy then delete.
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok || srcObj.fs.r != f.r {
		return nil, fs.ErrorCantMove
	}
	srcBkt, from := srcObj.fs.split(srcObj.remote)
	bkt, to := f.split(remote)
	if srcBkt != bkt || from == "" || to == "" {
		return nil, fs.ErrorCantMove
	}
	c, err := f.conn(ctx, bkt, false, fs.ErrorObjectNotFound)
	if err != nil {
		return nil, err
	}
	var e, old *fileEntry
	c.read(func(x *index) {
		e = x.Files[from]
		old = x.Files[to]
		if e == nil {
			err = fs.ErrorObjectNotFound
		} else if x.Dirs[to] {
			err = fs.ErrorIsDir
		}
	})
	if err != nil {
		return nil, err
	}
	entry := *e
	err = c.logOp(ctx, &meta{Op: opMove, FileID: entry.FileID, From: from, Path: to}, func(x *index) {
		if cur := x.Files[from]; cur != nil && cur.FileID == entry.FileID {
			delete(x.Files, from)
			x.Files[to] = cur
			x.addParents(to)
		}
	})
	if err != nil {
		return nil, err
	}
	if old != nil && old.FileID != entry.FileID {
		if err := c.st.deleteMessages(ctx, old.Parts); err != nil {
			fs.Debugf(f, "move: delete overwritten parts: %v", err)
		}
	}
	return f.newObject(remote, &entry), nil
}

// DirMove moves srcRemote in src to dstRemote in f. Inside a bucket only
// the index changes. Renaming a whole bucket only changes the registry.
// Other moves between buckets forward the messages.
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok || srcFs.r != f.r {
		return fs.ErrorCantDirMove
	}
	srcBkt, from := srcFs.split(srcRemote)
	bkt, to := f.split(dstRemote)
	if srcBkt == "" || bkt == "" {
		return fs.ErrorCantDirMove
	}

	// The destination must not exist.
	dc, err := f.r.conn(ctx, bkt, false)
	if err != nil && !errors.Is(err, errNoBucket) {
		return err
	}
	if dc != nil {
		exists := to == ""
		if !exists {
			dc.read(func(x *index) { exists = x.dirExists(to) })
		}
		if exists {
			return fs.ErrorDirExists
		}
	}
	if srcBkt == bkt && (from == "" || strings.HasPrefix(to, from+"/")) {
		return fs.ErrorCantDirMove
	}

	sc, err := srcFs.conn(ctx, srcBkt, false, fs.ErrorDirNotFound)
	if err != nil {
		return err
	}
	sc.read(func(x *index) {
		if !x.dirExists(from) {
			err = fs.ErrorDirNotFound
		}
	})
	if err != nil {
		return err
	}
	switch {
	case srcBkt == bkt:
		return sc.logOp(ctx, &meta{Op: opDirMove, From: from, Path: to}, func(x *index) {
			x.moveDir(from, to)
		})
	case from == "" && to == "":
		return f.r.renameBucket(ctx, srcBkt, bkt)
	}
	if dc, err = f.conn(ctx, bkt, true, nil); err != nil {
		return err
	}
	return f.moveAcross(ctx, sc, from, dc, to)
}

// moveAcross moves directory from in bucket sc to directory to in bucket
// dc by forwarding every file, then deletes the source.
func (f *Fs) moveAcross(ctx context.Context, sc *conn, from string, dc *conn, to string) error {
	// Pick up files other processes added, so none is left behind when
	// the source is deleted.
	if err := sc.sync(ctx); err != nil {
		return err
	}
	files := map[string]*fileEntry{}
	var dirs []string
	under := func(p string) bool { return from == "" || p == from || strings.HasPrefix(p, from+"/") }
	sc.read(func(x *index) {
		for p, e := range x.Files {
			if under(p) {
				cp := *e
				files[p] = &cp
			}
		}
		for d := range x.Dirs {
			if under(d) {
				dirs = append(dirs, d)
			}
		}
	})
	rebase := func(p string) string {
		rest := p
		if from != "" {
			rest = strings.TrimPrefix(p[len(from):], "/")
		}
		return cleanPath(path.Join(to, rest))
	}
	for p := range files {
		if err := checkCaption(rebase(p)); err != nil {
			return err
		}
	}

	if err := dc.mkdir(ctx, to); err != nil {
		return err
	}
	for _, d := range dirs {
		if err := dc.mkdir(ctx, rebase(d)); err != nil {
			return err
		}
	}
	for p, e := range files {
		if _, err := dc.copyFrom(ctx, sc, e, rebase(p)); err != nil {
			return err
		}
	}

	if from == "" {
		return f.r.deleteBucket(ctx, sc.bucket)
	}
	for p, e := range files {
		if err := sc.removeFile(ctx, p, e.FileID); err != nil && !errors.Is(err, fs.ErrorObjectNotFound) {
			return err
		}
	}
	for _, d := range dirs {
		if err := sc.logOp(ctx, &meta{Op: opRmdir, Path: d}, func(x *index) { delete(x.Dirs, d) }); err != nil {
			return err
		}
	}
	return nil
}

// Copy src to remote server-side by forwarding the messages, within a
// bucket or between buckets.
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok || srcObj.fs.r != f.r {
		return nil, fs.ErrorCantCopy
	}
	srcBkt, _ := srcObj.fs.split(srcObj.remote)
	bkt, full := f.split(remote)
	if full == "" {
		return nil, fs.ErrorCantCopy
	}
	if err := checkCaption(full); err != nil {
		return nil, err
	}
	sc, err := f.conn(ctx, srcBkt, false, fs.ErrorObjectNotFound)
	if err != nil {
		return nil, err
	}
	c, err := f.conn(ctx, bkt, true, nil)
	if err != nil {
		return nil, err
	}
	e, err := c.copyFrom(ctx, sc, &fileEntry{
		Size:    srcObj.size,
		ModTime: srcObj.modTime.UnixNano(),
		MD5:     srcObj.md5,
		Parts:   srcObj.parts,
	}, full)
	if err != nil {
		return nil, err
	}
	return f.newObject(remote, e), nil
}

// targetConns returns the conn of the root bucket, or of every bucket
// when the root has none.
func (f *Fs) targetConns(ctx context.Context) ([]*conn, error) {
	names := []string{}
	if bkt, _ := f.split(""); bkt != "" {
		names = append(names, bkt)
	} else {
		names = f.r.listBuckets(ctx)
	}
	conns := make([]*conn, 0, len(names))
	for _, n := range names {
		c, err := f.conn(ctx, n, false, fs.ErrorDirNotFound)
		if err != nil {
			return nil, fmt.Errorf("bucket %q: %w", n, err)
		}
		conns = append(conns, c)
	}
	return conns, nil
}

// Command runs backend-specific commands.
func (f *Fs) Command(ctx context.Context, name string, arg []string, opt map[string]string) (any, error) {
	var run func(c *conn, ctx context.Context) (int, error)
	var format string
	switch name {
	case "rebuild":
		run, format = (*conn).rebuild, "rebuilt index: %d files"
	case "cleanup":
		run, format = (*conn).cleanup, "deleted %d unused messages"
	default:
		return nil, fs.ErrorCommandNotFound
	}
	conns, err := f.targetConns(ctx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range conns {
		n, err := run(c, ctx)
		if err != nil {
			return nil, fmt.Errorf("bucket %q: %w", c.bucket, err)
		}
		out[f.opt.Enc.ToStandardName(c.bucket)] = fmt.Sprintf(format, n)
	}
	return out, nil
}

// CleanUp deletes parts left by failed or replaced uploads, and old index
// snapshots, in the root bucket or in every bucket. Unfinished uploads
// younger than 24h are kept, like unfinished multipart uploads on s3.
func (f *Fs) CleanUp(ctx context.Context) error {
	conns, err := f.targetConns(ctx)
	if err != nil {
		return err
	}
	for _, c := range conns {
		n, err := c.cleanup(ctx)
		if err != nil {
			return fmt.Errorf("bucket %q: %w", f.opt.Enc.ToStandardName(c.bucket), err)
		}
		fs.Infof(f, "bucket %q: deleted %d unused messages", f.opt.Enc.ToStandardName(c.bucket), n)
	}
	return nil
}

// Shutdown flushes pending index changes.
func (f *Fs) Shutdown(ctx context.Context) error {
	return f.r.flush(ctx)
}

// Check interfaces.
var (
	_ fs.Fs          = (*Fs)(nil)
	_ fs.PutStreamer = (*Fs)(nil)
	_ fs.Purger      = (*Fs)(nil)
	_ fs.Mover       = (*Fs)(nil)
	_ fs.DirMover    = (*Fs)(nil)
	_ fs.Copier      = (*Fs)(nil)
	_ fs.Commander   = (*Fs)(nil)
	_ fs.CleanUpper  = (*Fs)(nil)
	_ fs.Shutdowner  = (*Fs)(nil)
)
