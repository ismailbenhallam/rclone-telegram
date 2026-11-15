package telegram

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
)

// conn is shared by every Fs on the same bucket: one channel and one
// index.
//
// Every change is first written to the channel as a message, then applied
// to the live index. base is the index derived purely from the channel up
// to base.LastMsgID; sync replays newer messages into it and resets live
// to a copy, which also merges changes made by other processes.
type conn struct {
	r      *remote
	bucket string
	ref    channelRef
	opt    Options
	st     store

	// logMu orders writes against sync: writers hold it shared while
	// posting a message and updating live, sync holds it exclusively.
	logMu sync.RWMutex

	mu       sync.Mutex // protects the fields below
	base     *index
	live     *index
	dirty    bool
	dropped  bool
	timer    *time.Timer
	idxMsgID int
	lastSync time.Time

	flushMu sync.Mutex // serialises flushes
}

// drop stops background flushes of a conn whose bucket was deleted.
func (c *conn) drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	c.dirty = false
	c.dropped = true
}

// load reads the newest snapshot then catches up with the channel.
func (c *conn) load(ctx context.Context) error {
	hint := c.r.indexHint(c.ref)
	idx := newIndex()
	id, data, err := c.st.latestIndex(ctx, hint)
	if err == nil && id != 0 {
		idx, err = decodeIndex(data)
	}
	if err != nil {
		fs.Logf(nil, "telegram: bucket %q: could not read index snapshot, replaying the channel: %v", c.bucket, err)
		idx, id = newIndex(), 0
	}
	c.mu.Lock()
	c.base, c.live, c.idxMsgID = idx, idx.clone(), id
	c.mu.Unlock()
	return c.sync(ctx)
}

func decodeIndex(data []byte) (*index, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	idx := &index{}
	if err := json.NewDecoder(zr).Decode(idx); err != nil {
		return nil, err
	}
	return idx.init(), nil
}

// sync replays channel messages newer than the base index.
func (c *conn) sync(ctx context.Context) error {
	c.logMu.Lock()
	defer c.logMu.Unlock()
	c.mu.Lock()
	from := c.base.LastMsgID
	c.mu.Unlock()

	msgs, err := c.st.history(ctx, from)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.base.replay(msgs)
	c.live = c.base.clone()
	c.lastSync = time.Now()
	return nil
}

// maybeSync picks up changes from other processes every refresh_interval.
func (c *conn) maybeSync(ctx context.Context) {
	if c.opt.RefreshInterval <= 0 {
		return
	}
	c.mu.Lock()
	due := time.Since(c.lastSync) >= time.Duration(c.opt.RefreshInterval)
	c.mu.Unlock()
	if due {
		if err := c.sync(ctx); err != nil {
			fs.Debugf(nil, "telegram: refresh failed: %v", err)
		}
	}
}

// read runs fn with the live index locked.
func (c *conn) read(fn func(x *index)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	fn(c.live)
}

// logOp writes an op message then applies the change to the live index.
func (c *conn) logOp(ctx context.Context, m *meta, apply func(x *index)) error {
	c.logMu.RLock()
	defer c.logMu.RUnlock()
	if _, err := c.st.sendText(ctx, m.encode()); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	apply(c.live)
	c.markDirtyLocked()
	return nil
}

// mkdir records directory full. The bucket root always exists.
func (c *conn) mkdir(ctx context.Context, full string) error {
	if full == "" {
		return nil
	}
	var exists, isFile bool
	c.read(func(x *index) {
		_, isFile = x.Files[full]
		exists = x.Dirs[full]
	})
	if isFile {
		return fs.ErrorIsFile
	}
	if exists {
		return nil
	}
	return c.logOp(ctx, &meta{Op: opMkdir, Path: full}, func(x *index) {
		x.Dirs[full] = true
		x.addParents(full)
	})
}

// removeFile deletes the file at full if it still has fileID ("" matches
// any), then its messages.
func (c *conn) removeFile(ctx context.Context, full, fileID string) error {
	var e *fileEntry
	c.read(func(x *index) { e = x.Files[full] })
	if e == nil || (fileID != "" && e.FileID != fileID) {
		return fs.ErrorObjectNotFound
	}
	parts, id := e.Parts, e.FileID
	err := c.logOp(ctx, &meta{Op: opDelete, FileID: id, Path: full}, func(x *index) {
		if cur := x.Files[full]; cur != nil && cur.FileID == id {
			delete(x.Files, full)
		}
		delete(x.Pending, id)
	})
	if err != nil {
		return err
	}
	return c.st.deleteMessages(ctx, parts)
}

// copyFrom copies file e of bucket src to full in this bucket by
// forwarding its messages (src may be c itself). Empty files are text
// messages, so they are posted again instead.
func (c *conn) copyFrom(ctx context.Context, src *conn, e *fileEntry, full string) (*fileEntry, error) {
	ne := &fileEntry{FileID: randomHex(), Size: e.Size, ModTime: e.ModTime, MD5: e.MD5}

	// Hold the log lock until the captions are written, so a sync never
	// sees the copied parts half-labelled.
	c.logMu.RLock()
	var (
		ids []int
		err error
	)
	if e.Size == 0 {
		m := meta{FileID: ne.FileID, Path: full, ModTime: ne.ModTime, Last: true, MD5: ne.MD5}
		var id int
		if id, err = c.st.sendText(ctx, m.encode()); err == nil {
			ids = []int{id}
		}
	} else if ids, err = c.st.forward(ctx, src.st, e.Parts); err == nil {
		for i, id := range ids {
			m := meta{FileID: ne.FileID, Path: full, Part: i, ModTime: ne.ModTime}
			if i == len(ids)-1 {
				m.Last, m.Size, m.MD5 = true, ne.Size, ne.MD5
			}
			if err = c.st.editCaption(ctx, id, m.encode()); err != nil {
				break
			}
		}
	}
	var old *fileEntry
	if err == nil {
		ne.Parts = ids
		c.mu.Lock()
		old = c.live.Files[full]
		c.live.Files[full] = ne
		c.live.addParents(full)
		c.markDirtyLocked()
		c.mu.Unlock()
	}
	c.logMu.RUnlock()

	if err != nil {
		if len(ids) > 0 {
			_ = c.st.deleteMessages(context.Background(), ids)
		}
		return nil, err
	}
	if old != nil && old.FileID != ne.FileID {
		if err := c.st.deleteMessages(ctx, old.Parts); err != nil {
			fs.Debugf(nil, "telegram: copy: delete overwritten parts: %v", err)
		}
	}
	return ne, nil
}

// commit records a fully uploaded file in the live index and deletes the
// parts of the file it replaced. The data parts are already in the log.
func (c *conn) commit(ctx context.Context, full string, e *fileEntry) {
	c.logMu.RLock()
	c.mu.Lock()
	old := c.live.Files[full]
	c.live.Files[full] = e
	c.live.addParents(full)
	c.markDirtyLocked()
	c.mu.Unlock()
	c.logMu.RUnlock()
	if old != nil && old.FileID != e.FileID {
		if err := c.st.deleteMessages(ctx, old.Parts); err != nil {
			fs.Debugf(nil, "telegram: delete replaced parts: %v", err)
		}
	}
}

// forgetPending drops a failed upload from the pending lists.
func (c *conn) forgetPending(fileID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.base.Pending, fileID)
	delete(c.live.Pending, fileID)
}

func (c *conn) markDirtyLocked() {
	if c.dropped {
		return
	}
	c.dirty = true
	if c.timer == nil {
		c.timer = time.AfterFunc(time.Duration(c.opt.FlushInterval), func() {
			if err := c.flush(context.Background()); err != nil {
				fs.Errorf(nil, "telegram: bucket %q: index flush failed: %v", c.bucket, err)
			}
		})
	}
}

// flush catches up with the channel and uploads a new index snapshot.
func (c *conn) flush(ctx context.Context) error {
	c.flushMu.Lock()
	defer c.flushMu.Unlock()

	c.mu.Lock()
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	dirty := c.dirty
	c.mu.Unlock()
	if !dirty {
		return nil
	}
	if err := c.sync(ctx); err != nil {
		c.retryLater()
		return err
	}

	c.mu.Lock()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	err := json.NewEncoder(zw).Encode(c.base)
	if err == nil {
		err = zw.Close()
	}
	lastID, oldID := c.base.LastMsgID, c.idxMsgID
	c.dirty = false
	c.mu.Unlock()
	if err != nil {
		c.retryLater()
		return err
	}

	caption := fmt.Sprintf("%s {\"v\":1,\"last\":%d}", indexTag, lastID)
	const name = "rclone-index.json.gz"
	file, err := c.st.upload(ctx, name, bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	var id int
	if err == nil {
		id, err = c.st.sendFile(ctx, name, file, caption)
	}
	if err != nil {
		c.retryLater()
		return err
	}
	c.mu.Lock()
	c.idxMsgID = id
	c.mu.Unlock()
	c.r.setIndexHint(c.ref, id)
	if oldID != 0 {
		if err := c.st.deleteMessages(ctx, []int{oldID}); err != nil {
			fs.Debugf(nil, "telegram: delete old index: %v", err)
		}
	}
	return nil
}

func (c *conn) retryLater() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markDirtyLocked()
}

// rebuild replays the whole channel from the first message.
func (c *conn) rebuild(ctx context.Context) (int, error) {
	c.mu.Lock()
	c.base = newIndex()
	c.dirty = true
	c.mu.Unlock()
	if err := c.sync(ctx); err != nil {
		return 0, err
	}
	if err := c.flush(ctx); err != nil {
		return 0, err
	}
	n := 0
	c.read(func(x *index) { n = len(x.Files) })
	return n, nil
}

// orphanAge is how old an unfinished upload must be before cleanup
// deletes it, so uploads in progress elsewhere are left alone.
const orphanAge = 24 * time.Hour

// cleanup deletes data messages no file refers to (failed or replaced
// uploads) and stale index snapshots.
func (c *conn) cleanup(ctx context.Context) (int, error) {
	if err := c.sync(ctx); err != nil {
		return 0, err
	}
	msgs, err := c.st.history(ctx, 0)
	if err != nil {
		return 0, err
	}

	c.mu.Lock()
	used := map[int]bool{c.idxMsgID: true}
	for _, e := range c.live.Files {
		for _, id := range e.Parts {
			used[id] = true
		}
	}
	newest := map[string]time.Time{}
	for _, msg := range msgs {
		if m, ok := parseMeta(msg.Text); ok && m.Op == "" && msg.Date.After(newest[m.FileID]) {
			newest[m.FileID] = msg.Date
		}
	}
	var del []int
	dropped := map[string]bool{}
	for _, msg := range msgs {
		if used[msg.ID] || msg.ID > c.base.LastMsgID {
			continue
		}
		if strings.HasPrefix(msg.Text, indexTag) {
			del = append(del, msg.ID)
			continue
		}
		m, ok := parseMeta(msg.Text)
		if !ok || m.Op != "" {
			continue
		}
		if _, pending := c.live.Pending[m.FileID]; pending && time.Since(newest[m.FileID]) < orphanAge {
			continue
		}
		del = append(del, msg.ID)
		dropped[m.FileID] = true
	}
	for f := range dropped {
		delete(c.base.Pending, f)
		delete(c.live.Pending, f)
	}
	if len(dropped) > 0 {
		c.markDirtyLocked()
	}
	c.mu.Unlock()

	if err := c.st.deleteMessages(ctx, del); err != nil {
		return 0, err
	}
	return len(del), nil
}

func randomHex() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%x", b)
}
