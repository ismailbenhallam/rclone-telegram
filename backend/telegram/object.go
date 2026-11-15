package telegram

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
)

// Object is a file stored as one or more messages in its bucket channel.
type Object struct {
	fs      *Fs
	remote  string
	size    int64
	modTime time.Time
	md5     string
	parts   []int
	fileID  string
}

// Fs returns the parent Fs.
func (o *Object) Fs() fs.Info { return o.fs }

// Remote returns the remote path.
func (o *Object) Remote() string { return o.remote }

// String returns a description of the Object.
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// ModTime returns the modification time.
func (o *Object) ModTime(context.Context) time.Time { return o.modTime }

// Size returns the size in bytes.
func (o *Object) Size() int64 { return o.size }

// Storable returns true: all objects can be stored.
func (o *Object) Storable() bool { return true }

// Hash returns the MD5 of the object.
func (o *Object) Hash(_ context.Context, t hash.Type) (string, error) {
	if t != hash.MD5 {
		return "", hash.ErrUnsupported
	}
	return o.md5, nil
}

// locate returns the conn of the object's bucket and its encoded path in
// it. With create, a missing bucket is created.
func (o *Object) locate(ctx context.Context, create bool) (*conn, string, error) {
	bkt, full := o.fs.split(o.remote)
	if full == "" {
		return nil, "", fmt.Errorf("telegram: %q is not a file inside a bucket", o.remote)
	}
	c, err := o.fs.conn(ctx, bkt, create, fs.ErrorObjectNotFound)
	return c, full, err
}

// SetModTime sets the modification time.
func (o *Object) SetModTime(ctx context.Context, t time.Time) error {
	c, full, err := o.locate(ctx, false)
	if err != nil {
		return err
	}
	found := false
	c.read(func(x *index) {
		e := x.Files[full]
		found = e != nil && e.FileID == o.fileID
	})
	if !found {
		return fs.ErrorObjectNotFound
	}
	m := t.UnixNano()
	err = c.logOp(ctx, &meta{Op: opSetMtime, FileID: o.fileID, Path: full, ModTime: m}, func(x *index) {
		if e := x.Files[full]; e != nil && e.FileID == o.fileID {
			e.ModTime = m
		}
	})
	if err == nil {
		o.modTime = t
	}
	return err
}

// Remove deletes the object.
func (o *Object) Remove(ctx context.Context) error {
	c, full, err := o.locate(ctx, false)
	if err != nil {
		return err
	}
	return c.removeFile(ctx, full, "")
}

// countingReader counts bytes read.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// checkCaption fails early if the path cannot fit in a caption.
func checkCaption(full string) error {
	m := meta{FileID: strings.Repeat("f", 16), Path: full, Part: 9999, ModTime: 1 << 62,
		Last: true, Size: 1 << 62, MD5: strings.Repeat("0", 32)}
	if n := utf8.RuneCountInString(m.encode()); n > captionLimit {
		return fmt.Errorf("telegram: path too long to store (%d of %d caption characters): %q", n, captionLimit, full)
	}
	return nil
}

// Update uploads new content, replacing the old.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (err error) {
	f := o.fs
	c, full, err := o.locate(ctx, true)
	if err != nil {
		return err
	}
	if err := checkCaption(full); err != nil {
		return err
	}
	modTime := src.ModTime(ctx).UnixNano()
	fileID := randomHex()
	chunk := int64(f.opt.ChunkSize)
	size := src.Size()

	h := md5.New()
	br := bufio.NewReaderSize(io.TeeReader(in, h), 1<<20)

	var parts []int
	defer func() {
		if err != nil {
			if len(parts) > 0 {
				if derr := c.st.deleteMessages(context.Background(), parts); derr != nil {
					fs.Debugf(o, "cleanup after failed upload: %v", derr)
				}
			}
			c.forgetPending(fileID)
		}
	}()

	empty := size == 0
	if size < 0 {
		if _, perr := br.Peek(1); perr == io.EOF {
			empty = true
		} else if perr != nil {
			return perr
		}
	}

	var total int64
	if empty {
		// Telegram rejects empty documents: store a text message instead.
		m := meta{FileID: fileID, Path: full, ModTime: modTime, Last: true, MD5: hex.EncodeToString(h.Sum(nil))}
		id, err := c.st.sendText(ctx, m.encode())
		if err != nil {
			return err
		}
		parts = append(parts, id)
	}

	for i := 0; !empty; i++ {
		partLen := int64(-1)
		if size >= 0 {
			partLen = min(chunk, size-total)
		}
		cr := &countingReader{r: io.LimitReader(br, chunk)}
		name := path.Base(full)
		if size < 0 || size > chunk {
			name = fmt.Sprintf("%s.part%03d", name, i)
		}
		file, err := c.st.upload(ctx, name, cr, partLen)
		if err != nil {
			return err
		}
		total += cr.n
		if size >= 0 && cr.n != partLen {
			return fmt.Errorf("telegram: short upload: got %d of %d bytes: %w", total, size, io.ErrUnexpectedEOF)
		}

		// Only now do we know whether this is the last part.
		last := false
		if size >= 0 {
			last = total >= size
		} else if _, perr := br.Peek(1); perr == io.EOF {
			last = true
		} else if perr != nil {
			return perr
		}

		m := meta{FileID: fileID, Path: full, Part: i, ModTime: modTime}
		if last {
			m.Last, m.Size, m.MD5 = true, total, hex.EncodeToString(h.Sum(nil))
		}
		id, err := c.st.sendFile(ctx, name, file, m.encode())
		if err != nil {
			return err
		}
		parts = append(parts, id)
		if last {
			break
		}
	}
	if size >= 0 && total != size {
		return fmt.Errorf("telegram: short upload: got %d of %d bytes", total, size)
	}

	e := &fileEntry{FileID: fileID, Size: total, ModTime: modTime, MD5: hex.EncodeToString(h.Sum(nil)), Parts: parts}
	c.commit(ctx, full, e)
	o.size, o.modTime, o.md5, o.parts, o.fileID = e.Size, time.Unix(0, modTime), e.MD5, parts, fileID
	return nil
}

// Open returns a reader for the object, honouring range/seek options.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	offset, limit := int64(0), int64(-1)
	for _, opt := range options {
		switch x := opt.(type) {
		case *fs.SeekOption:
			offset = x.Offset
		case *fs.RangeOption:
			offset, limit = x.Decode(o.size)
		default:
			if opt.Mandatory() {
				fs.Logf(o, "unsupported mandatory option: %v", opt)
			}
		}
	}
	end := o.size
	if limit >= 0 && offset+limit < end {
		end = offset + limit
	}
	c, _, err := o.locate(ctx, false)
	if err != nil {
		return nil, err
	}
	r := &reader{ctx: ctx, st: c.st, ids: o.parts, pos: offset, end: end}
	if offset >= end {
		return r, nil
	}
	if err := r.resolve(); err != nil {
		return nil, err
	}
	return r, nil
}

// reader streams a byte range across the parts of an object.
type reader struct {
	ctx   context.Context
	st    store
	ids   []int
	parts []part
	pos   int64 // position of the next byte to return
	end   int64 // exclusive
	buf   []byte
}

func (r *reader) resolve() (err error) {
	r.parts, err = r.st.resolve(r.ctx, r.ids)
	return err
}

func (r *reader) fill() error {
	var start int64
	for _, p := range r.parts {
		if r.pos < start+p.size {
			local := r.pos - start
			aligned := local - local%blockSize
			b, err := r.st.readBlock(r.ctx, p, aligned)
			if errors.Is(err, errRefExpired) {
				if err = r.resolve(); err == nil {
					return r.fill()
				}
			}
			if err != nil {
				return err
			}
			skip := local - aligned
			if int64(len(b)) <= skip {
				return io.ErrUnexpectedEOF
			}
			r.buf = b[skip:]
			return nil
		}
		start += p.size
	}
	return io.ErrUnexpectedEOF
}

func (r *reader) Read(p []byte) (int, error) {
	if r.pos >= r.end {
		return 0, io.EOF
	}
	if len(r.buf) == 0 {
		if err := r.fill(); err != nil {
			return 0, err
		}
	}
	n := copy(p, r.buf[:min(int64(len(r.buf)), r.end-r.pos)])
	r.buf = r.buf[n:]
	r.pos += int64(n)
	return n, nil
}

func (r *reader) Close() error { return nil }

// Check interfaces.
var _ fs.Object = (*Object)(nil)
