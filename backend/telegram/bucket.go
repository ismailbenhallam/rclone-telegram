package telegram

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/lib/atexit"
)

// newAccount opens the Telegram account. Tests replace it with a fake.
var newAccount = newTGAccount

// bucketTag prefixes registry messages.
const bucketTag = "#rclone_bucket "

// Registry operations.
const (
	opBucketAdd = "add" // Name, ID, Hash
	opBucketDel = "del" // Name, ID
	opBucketMv  = "mv"  // Name -> To, ID
)

// maxBucketName is the longest bucket name accepted, in characters.
const maxBucketName = 100

// errNoBucket means the bucket does not exist.
var errNoBucket = errors.New("telegram: bucket not found")

// bucketOp is a registry message. The registry channel is an append-only
// log of these: replaying it in ID order gives the bucket list.
type bucketOp struct {
	Op   string `json:"op"`
	Name string `json:"name"`
	ID   int64  `json:"id"`
	Hash int64  `json:"hash,omitempty"`
	To   string `json:"to,omitempty"`
}

func (o *bucketOp) encode() string {
	b, _ := json.Marshal(o)
	return bucketTag + string(b)
}

func parseBucketOp(s string) (*bucketOp, bool) {
	if !strings.HasPrefix(s, bucketTag) {
		return nil, false
	}
	var o bucketOp
	if err := json.Unmarshal([]byte(s[len(bucketTag):]), &o); err != nil {
		return nil, false
	}
	if o.Name == "" || o.ID == 0 {
		return nil, false
	}
	switch o.Op {
	case opBucketAdd, opBucketDel:
	case opBucketMv:
		if o.To == "" {
			return nil, false
		}
	default:
		return nil, false
	}
	return &o, true
}

// remote is shared by every Fs on the same configured remote: one
// Telegram login, the bucket registry and one conn per open bucket.
type remote struct {
	name string
	m    configmap.Mapper
	opt  Options
	acct account

	regMu sync.Mutex // serialises registry syncs

	mu       sync.Mutex // protects the fields below
	buckets  map[string]channelRef
	regLast  int
	lastSync time.Time

	connMu sync.Mutex // protects conns and serialises bucket create/delete
	conns  map[string]*conn

	hintMu sync.Mutex // serialises index_msg_ids updates
}

var (
	remotesMu sync.Mutex
	remotes   = map[string]*remote{}
)

func getRemote(ctx context.Context, name string, opt *Options, m configmap.Mapper) (*remote, error) {
	remotesMu.Lock()
	defer remotesMu.Unlock()
	if r, ok := remotes[name]; ok {
		return r, nil
	}
	acct, err := newAccount(ctx, name, opt, m)
	if err != nil {
		return nil, err
	}
	r := &remote{
		name:    name,
		m:       m,
		opt:     *opt,
		acct:    acct,
		buckets: map[string]channelRef{},
		conns:   map[string]*conn{},
	}
	if err := r.syncRegistry(ctx); err != nil {
		_ = acct.close()
		return nil, err
	}
	atexit.Register(func() {
		if err := r.flush(context.Background()); err != nil {
			fs.Errorf(nil, "telegram: final index flush failed: %v", err)
		}
	})
	remotes[name] = r
	return r, nil
}

// syncRegistry replays registry messages newer than the last one seen.
// When two processes add the same bucket, the first add wins.
func (r *remote) syncRegistry(ctx context.Context) error {
	r.regMu.Lock()
	defer r.regMu.Unlock()
	r.mu.Lock()
	from := r.regLast
	r.mu.Unlock()

	msgs, err := r.acct.registry().history(ctx, from)
	if err != nil {
		return err
	}
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, msg := range msgs {
		if msg.ID <= r.regLast {
			continue
		}
		r.regLast = msg.ID
		op, ok := parseBucketOp(msg.Text)
		if !ok {
			continue
		}
		cur, exists := r.buckets[op.Name]
		switch {
		case op.Op == opBucketAdd && !exists:
			r.buckets[op.Name] = channelRef{ID: op.ID, Hash: op.Hash}
		case op.Op == opBucketDel && exists && cur.ID == op.ID:
			delete(r.buckets, op.Name)
		case op.Op == opBucketMv && exists && cur.ID == op.ID:
			if _, taken := r.buckets[op.To]; !taken {
				delete(r.buckets, op.Name)
				r.buckets[op.To] = cur
			}
		}
	}
	r.lastSync = time.Now()
	return nil
}

// maybeSyncRegistry picks up buckets added or deleted by other processes
// every refresh_interval.
func (r *remote) maybeSyncRegistry(ctx context.Context) {
	if r.opt.RefreshInterval <= 0 {
		return
	}
	r.mu.Lock()
	due := time.Since(r.lastSync) >= time.Duration(r.opt.RefreshInterval)
	r.mu.Unlock()
	if due {
		if err := r.syncRegistry(ctx); err != nil {
			fs.Debugf(nil, "telegram: registry refresh failed: %v", err)
		}
	}
}

// ref returns the channel of a bucket, as currently known.
func (r *remote) ref(name string) (channelRef, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ref, ok := r.buckets[name]
	return ref, ok
}

// lookup returns the channel of a bucket, syncing the registry once if
// the bucket is not known yet.
func (r *remote) lookup(ctx context.Context, name string) (channelRef, bool, error) {
	if ref, ok := r.ref(name); ok {
		return ref, true, nil
	}
	if err := r.syncRegistry(ctx); err != nil {
		return channelRef{}, false, err
	}
	ref, ok := r.ref(name)
	return ref, ok, nil
}

// listBuckets returns the sorted names of every bucket.
func (r *remote) listBuckets(ctx context.Context) []string {
	r.maybeSyncRegistry(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	names := make([]string, 0, len(r.buckets))
	for n := range r.buckets {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// conn returns the conn of a bucket. A missing bucket is created if
// create is set, otherwise errNoBucket is returned.
func (r *remote) conn(ctx context.Context, name string, create bool) (*conn, error) {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	if c, ok := r.conns[name]; ok {
		if ref, ok := r.ref(name); ok && ref == c.ref {
			return c, nil
		}
		// Deleted, maybe by another process.
		c.drop()
		delete(r.conns, name)
	}
	ref, ok, err := r.lookup(ctx, name)
	if err != nil {
		return nil, err
	}
	if !ok {
		if !create {
			return nil, errNoBucket
		}
		if ref, err = r.createBucket(ctx, name); err != nil {
			return nil, err
		}
	}
	c := &conn{r: r, bucket: name, ref: ref, opt: r.opt, st: r.acct.channel(ref)}
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	r.conns[name] = c
	return c, nil
}

func checkBucketName(name string) error {
	if n := utf8.RuneCountInString(name); n == 0 || n > maxBucketName {
		return fmt.Errorf("telegram: bucket name must be 1 to %d characters: %q", maxBucketName, name)
	}
	return nil
}

// createBucket creates the channel of a bucket and records it in the
// registry. If another process created the same bucket first, its
// channel is used and ours is deleted. Call with connMu held.
func (r *remote) createBucket(ctx context.Context, name string) (channelRef, error) {
	if err := checkBucketName(name); err != nil {
		return channelRef{}, err
	}
	ref, err := r.acct.createChannel(ctx, r.opt.ChannelTitle+"/"+name)
	if err != nil {
		return channelRef{}, err
	}
	op := bucketOp{Op: opBucketAdd, Name: name, ID: ref.ID, Hash: ref.Hash}
	if _, err := r.acct.registry().sendText(ctx, op.encode()); err != nil {
		_ = r.acct.deleteChannel(context.Background(), ref)
		return channelRef{}, err
	}
	if err := r.syncRegistry(ctx); err != nil {
		return channelRef{}, err
	}
	won, ok := r.ref(name)
	if !ok {
		return channelRef{}, fmt.Errorf("telegram: bucket %q vanished while being created", name)
	}
	if won != ref {
		fs.Debugf(nil, "telegram: bucket %q was created concurrently, using the other channel", name)
		if err := r.acct.deleteChannel(ctx, ref); err != nil {
			fs.Logf(nil, "telegram: delete duplicate channel %d: %v", ref.ID, err)
		}
		return won, nil
	}
	fs.Logf(nil, "telegram: created bucket %q (channel %d)", name, ref.ID)
	return ref, nil
}

// deleteBucket removes a bucket from the registry, then deletes its
// channel with every message in it.
func (r *remote) deleteBucket(ctx context.Context, name string) error {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	ref, ok, err := r.lookup(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fs.ErrorDirNotFound
	}
	op := bucketOp{Op: opBucketDel, Name: name, ID: ref.ID}
	if _, err := r.acct.registry().sendText(ctx, op.encode()); err != nil {
		return err
	}
	if c, ok := r.conns[name]; ok {
		c.drop()
		delete(r.conns, name)
	}
	r.mu.Lock()
	if cur, ok := r.buckets[name]; ok && cur == ref {
		delete(r.buckets, name)
	}
	r.mu.Unlock()
	r.forgetIndexHint(ref)
	return r.acct.deleteChannel(ctx, ref)
}

// renameBucket renames a bucket by recording one registry message. The
// channel and its messages stay as they are.
func (r *remote) renameBucket(ctx context.Context, from, to string) error {
	if err := checkBucketName(to); err != nil {
		return err
	}
	r.connMu.Lock()
	defer r.connMu.Unlock()
	ref, ok, err := r.lookup(ctx, from)
	if err != nil {
		return err
	}
	if !ok {
		return fs.ErrorDirNotFound
	}
	if c, ok := r.conns[from]; ok {
		if err := c.flush(ctx); err != nil {
			return err
		}
		c.drop()
		delete(r.conns, from)
	}
	op := bucketOp{Op: opBucketMv, Name: from, ID: ref.ID, To: to}
	if _, err := r.acct.registry().sendText(ctx, op.encode()); err != nil {
		return err
	}
	if err := r.syncRegistry(ctx); err != nil {
		return err
	}
	if cur, _ := r.ref(to); cur != ref {
		return fs.ErrorDirExists // another process took the name first
	}
	if err := r.acct.setTitle(ctx, ref, r.opt.ChannelTitle+"/"+to); err != nil {
		fs.Debugf(nil, "telegram: rename channel %d: %v", ref.ID, err)
	}
	return nil
}

// openConns returns the conns opened so far.
func (r *remote) openConns() []*conn {
	r.connMu.Lock()
	defer r.connMu.Unlock()
	out := make([]*conn, 0, len(r.conns))
	for _, c := range r.conns {
		out = append(out, c)
	}
	return out
}

// flush saves pending index changes of every open bucket.
func (r *remote) flush(ctx context.Context) error {
	var errs []error
	for _, c := range r.openConns() {
		if err := c.flush(ctx); err != nil {
			errs = append(errs, fmt.Errorf("bucket %q: %w", c.bucket, err))
		}
	}
	return errors.Join(errs...)
}

// Index snapshot hints are kept in the config as a JSON map from channel
// ID to message ID, so startup finds the snapshot without searching.

func (r *remote) hints() map[string]int {
	out := map[string]int{}
	if v, ok := r.m.Get("index_msg_ids"); ok && v != "" {
		_ = json.Unmarshal([]byte(v), &out)
	}
	return out
}

func (r *remote) indexHint(ref channelRef) int {
	r.hintMu.Lock()
	defer r.hintMu.Unlock()
	return r.hints()[strconv.FormatInt(ref.ID, 10)]
}

func (r *remote) setIndexHint(ref channelRef, id int) {
	r.hintMu.Lock()
	defer r.hintMu.Unlock()
	h := r.hints()
	h[strconv.FormatInt(ref.ID, 10)] = id
	b, _ := json.Marshal(h)
	r.m.Set("index_msg_ids", string(b))
}

func (r *remote) forgetIndexHint(ref channelRef) {
	r.hintMu.Lock()
	defer r.hintMu.Unlock()
	h := r.hints()
	key := strconv.FormatInt(ref.ID, 10)
	if _, ok := h[key]; !ok {
		return
	}
	delete(h, key)
	b, _ := json.Marshal(h)
	r.m.Set("index_msg_ids", string(b))
}
