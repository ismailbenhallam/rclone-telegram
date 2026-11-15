package telegram

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
)

// memAccount is an in-memory account used to run the backend without
// Telegram.
type memAccount struct {
	mu    sync.Mutex
	reg   *memStore
	chans map[int64]*memStore
	next  int64
}

// memStore is an in-memory channel.
type memStore struct {
	mu   sync.Mutex
	next int
	msgs map[int]*memMsg
	now  func() time.Time
	gone bool // channel deleted
}

func newMemStore() *memStore {
	return &memStore{msgs: map[int]*memMsg{}, now: time.Now}
}

type memMsg struct {
	text  string
	data  []byte
	isDoc bool
	date  time.Time
}

var (
	memAccountsMu sync.Mutex
	memAccounts   = map[string]*memAccount{}
)

// fakeAccount returns the shared in-memory account named by
// channel_title, so several remotes can act as separate processes on one
// account.
func fakeAccount(_ context.Context, _ string, opt *Options, _ configmap.Mapper) (account, error) {
	memAccountsMu.Lock()
	defer memAccountsMu.Unlock()
	a := memAccounts[opt.ChannelTitle]
	if a == nil {
		a = &memAccount{reg: newMemStore(), chans: map[int64]*memStore{}}
		memAccounts[opt.ChannelTitle] = a
	}
	return a, nil
}

func (a *memAccount) registry() store { return a.reg }

func (a *memAccount) channel(ref channelRef) store {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.chans[ref.ID]; s != nil {
		return s
	}
	return &memStore{gone: true}
}

func (a *memAccount) createChannel(context.Context, string) (channelRef, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.next++
	a.chans[a.next] = newMemStore()
	return channelRef{ID: a.next, Hash: a.next * 7}, nil
}

func (a *memAccount) setTitle(context.Context, channelRef, string) error { return nil }

func (a *memAccount) deleteChannel(_ context.Context, ref channelRef) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if s := a.chans[ref.ID]; s != nil {
		s.mu.Lock()
		s.gone, s.msgs = true, map[int]*memMsg{}
		s.mu.Unlock()
		delete(a.chans, ref.ID)
	}
	return nil
}

func (a *memAccount) close() error { return nil }

// channelCount returns the number of live bucket channels.
func (a *memAccount) channelCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.chans)
}

var errGone = errors.New("fake: CHANNEL_INVALID")

func (s *memStore) add(m *memMsg) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return 0, errGone
	}
	s.next++
	m.date = s.now()
	s.msgs[s.next] = m
	return s.next, nil
}

func (s *memStore) upload(_ context.Context, _ string, r io.Reader, _ int64) (any, error) {
	return io.ReadAll(r)
}

func (s *memStore) sendFile(_ context.Context, _ string, file any, caption string) (int, error) {
	return s.add(&memMsg{text: caption, data: file.([]byte), isDoc: true})
}

func (s *memStore) sendText(_ context.Context, text string) (int, error) {
	return s.add(&memMsg{text: text})
}

func (s *memStore) deleteMessages(_ context.Context, ids []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		delete(s.msgs, id)
	}
	return nil
}

func (s *memStore) editCaption(_ context.Context, id int, caption string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.msgs[id]; m != nil {
		m.text = caption
	}
	return nil
}

func (s *memStore) forward(_ context.Context, src store, ids []int) ([]int, error) {
	from := src.(*memStore)
	out := make([]int, len(ids))
	for i, id := range ids {
		from.mu.Lock()
		m := from.msgs[id]
		from.mu.Unlock()
		if m == nil {
			return nil, fs.ErrorObjectNotFound
		}
		var err error
		if out[i], err = s.add(&memMsg{data: m.data, isDoc: m.isDoc}); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (s *memStore) resolve(_ context.Context, ids []int) ([]part, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts := make([]part, len(ids))
	for i, id := range ids {
		m := s.msgs[id]
		if m == nil || !m.isDoc {
			return nil, fs.ErrorObjectNotFound
		}
		parts[i] = part{ref: id, size: int64(len(m.data))}
	}
	return parts, nil
}

func (s *memStore) readBlock(_ context.Context, p part, offset int64) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.msgs[p.ref.(int)]
	if m == nil {
		return nil, fs.ErrorObjectNotFound
	}
	end := min(offset+blockSize, int64(len(m.data)))
	return append([]byte(nil), m.data[offset:end]...), nil
}

func (s *memStore) history(_ context.Context, minID int) ([]rawMsg, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.gone {
		return nil, errGone
	}
	var out []rawMsg
	for id, m := range s.msgs {
		if id > minID {
			out = append(out, rawMsg{ID: id, Text: m.text, Date: m.date})
		}
	}
	return out, nil
}

func (s *memStore) latestIndex(_ context.Context, hintID int) (int, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.msgs[hintID]; m != nil && strings.HasPrefix(m.text, indexTag) {
		return hintID, m.data, nil
	}
	best := 0
	for id, m := range s.msgs {
		if id > best && strings.HasPrefix(m.text, indexTag) {
			best = id
		}
	}
	if best == 0 {
		return 0, nil, nil
	}
	return best, s.msgs[best].data, nil
}

var (
	_ account = (*memAccount)(nil)
	_ store   = (*memStore)(nil)
)
