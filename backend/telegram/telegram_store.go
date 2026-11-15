package telegram

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/pool"
	"github.com/gotd/td/rpc"
	"github.com/gotd/td/session"
	"github.com/gotd/td/telegram"
	"github.com/gotd/td/telegram/uploader"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/fserrors"
)

// configSession stores the MTProto session in the rclone config.
type configSession struct {
	mu sync.Mutex
	m  configmap.Mapper
}

func (s *configSession) LoadSession(context.Context) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.m.Get("session")
	if !ok || v == "" {
		return nil, session.ErrNotFound
	}
	return base64.StdEncoding.DecodeString(v)
}

func (s *configSession) StoreSession(_ context.Context, data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m.Set("session", base64.StdEncoding.EncodeToString(data))
	return nil
}

// retryMiddleware waits out FLOOD_WAIT and retries transient server and
// network errors. Sends carry a fixed random_id, so a retry cannot post
// the same message twice.
func retryMiddleware(maxRetries int) telegram.Middleware {
	return telegram.MiddlewareFunc(func(next tg.Invoker) telegram.InvokeFunc {
		return func(ctx context.Context, in bin.Encoder, out bin.Decoder) error {
			backoff := time.Second
			for attempt := 0; ; attempt++ {
				err := next.Invoke(ctx, in, out)
				if err == nil || attempt >= maxRetries || ctx.Err() != nil {
					return err
				}
				var wait time.Duration
				if d, ok := tgerr.AsFloodWait(err); ok {
					wait = d + time.Second
				} else if rpcErr, ok := tgerr.As(err); (ok && (rpcErr.Code >= 500 || rpcErr.Code == -503)) || isNetworkError(err) {
					wait, backoff = backoff, min(backoff*2, 30*time.Second)
				} else {
					return err
				}
				fs.Debugf(nil, "telegram: %v, retrying in %v", err, wait)
				select {
				case <-time.After(wait):
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
	})
}

// isNetworkError reports whether err is a dropped connection or another
// network failure that is worth retrying. RPC errors are not: the server
// answered.
func isNetworkError(err error) bool {
	if _, ok := tgerr.As(err); ok {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	return errors.Is(err, pool.ErrConnDead) || errors.Is(err, rpc.ErrEngineClosed) || fserrors.ShouldRetry(err)
}

func connect(ctx context.Context, opt *Options, m configmap.Mapper) (*telegram.Client, error) {
	client, err := telegram.Connect(ctx, opt.APIID, opt.APIHash, telegram.Options{
		SessionStorage: &configSession{m: m},
		NoUpdates:      true,
		Middlewares:    []telegram.Middleware{retryMiddleware(5)},
	})
	if err != nil {
		return nil, fmt.Errorf("telegram: connect: %w", err)
	}
	return client, nil
}

// tgAccount implements account with one MTProto client shared by the
// registry channel and every bucket channel.
type tgAccount struct {
	client  *telegram.Client
	api     *tg.Client
	threads int
	reg     *tgStore
}

// tgStore implements store with a Telegram channel.
type tgStore struct {
	client  *telegram.Client
	api     *tg.Client
	channel *tg.InputChannel
	peer    *tg.InputPeerChannel
	threads int
}

// newTGAccount connects, checks the login and makes sure the registry
// channel exists.
func newTGAccount(ctx context.Context, name string, opt *Options, m configmap.Mapper) (account, error) {
	reconnect := fmt.Sprintf("run: rclone config reconnect %s:", name)
	if v, _ := m.Get("session"); v == "" {
		return nil, fmt.Errorf("telegram: not logged in, %s", reconnect)
	}
	client, err := connect(ctx, opt, m)
	if err != nil {
		return nil, err
	}
	status, err := client.Auth().Status(ctx)
	if err != nil || !status.Authorized {
		_ = client.Close()
		if err == nil {
			err = errors.New("session expired or revoked")
		}
		return nil, fmt.Errorf("telegram: %v, %s", err, reconnect)
	}
	a := &tgAccount{client: client, api: client.API(), threads: opt.UploadConcurrency}
	if err := a.ensureRegistry(ctx, opt, m); err != nil {
		_ = client.Close()
		return nil, err
	}
	return a, nil
}

// ensureRegistry creates the registry channel on first use.
func (a *tgAccount) ensureRegistry(ctx context.Context, opt *Options, m configmap.Mapper) error {
	if opt.ChannelID == 0 {
		ref, err := a.createChannel(ctx, opt.ChannelTitle)
		if err != nil {
			return err
		}
		opt.ChannelID, opt.ChannelAccessHash = ref.ID, ref.Hash
		m.Set("channel_id", strconv.FormatInt(ref.ID, 10))
		m.Set("channel_access_hash", strconv.FormatInt(ref.Hash, 10))
		fs.Logf(nil, "telegram: created private registry channel %q (id %d)", opt.ChannelTitle, ref.ID)
	} else {
		ok, err := channelReachable(ctx, a.api, opt.ChannelID, opt.ChannelAccessHash)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("telegram: this account cannot access storage channel %d. "+
				"If you switched accounts, run: rclone config reconnect <remote>: (a new channel is then created)", opt.ChannelID)
		}
	}
	a.reg = a.channel(channelRef{ID: opt.ChannelID, Hash: opt.ChannelAccessHash}).(*tgStore)
	return nil
}

func (a *tgAccount) registry() store { return a.reg }

func (a *tgAccount) channel(ref channelRef) store {
	return &tgStore{
		client:  a.client,
		api:     a.api,
		channel: &tg.InputChannel{ChannelID: ref.ID, AccessHash: ref.Hash},
		peer:    &tg.InputPeerChannel{ChannelID: ref.ID, AccessHash: ref.Hash},
		threads: a.threads,
	}
}

// channelTitle truncates a title to Telegram's 128 character limit.
func channelTitle(title string) string {
	if r := []rune(title); len(r) > 128 {
		return string(r[:128])
	}
	return title
}

func (a *tgAccount) setTitle(ctx context.Context, ref channelRef, title string) error {
	_, err := a.api.ChannelsEditTitle(ctx, &tg.ChannelsEditTitleRequest{
		Channel: &tg.InputChannel{ChannelID: ref.ID, AccessHash: ref.Hash},
		Title:   channelTitle(title),
	})
	if err != nil && !tgerr.Is(err, "CHAT_NOT_MODIFIED") {
		return fmt.Errorf("telegram: rename channel: %w", err)
	}
	return nil
}

func (a *tgAccount) createChannel(ctx context.Context, title string) (channelRef, error) {
	upd, err := a.api.ChannelsCreateChannel(ctx, &tg.ChannelsCreateChannelRequest{
		Broadcast: true,
		Title:     channelTitle(title),
		About:     "rclone storage. Do not post here manually.",
	})
	if err != nil {
		return channelRef{}, fmt.Errorf("telegram: create channel: %w", err)
	}
	for _, ch := range updatesChats(upd) {
		if channel, ok := ch.(*tg.Channel); ok {
			return channelRef{ID: channel.ID, Hash: channel.AccessHash}, nil
		}
	}
	return channelRef{}, errors.New("telegram: create channel: no channel in response")
}

func (a *tgAccount) deleteChannel(ctx context.Context, ref channelRef) error {
	_, err := a.api.ChannelsDeleteChannel(ctx, &tg.InputChannel{ChannelID: ref.ID, AccessHash: ref.Hash})
	if err != nil && !tgerr.Is(err, "CHANNEL_INVALID", "CHANNEL_PRIVATE") {
		return fmt.Errorf("telegram: delete channel: %w", err)
	}
	return nil
}

func (a *tgAccount) close() error { return a.client.Close() }

// channelReachable reports whether the logged-in account can use the
// channel. Access hashes are per account, so another account's channel
// fails with CHANNEL_INVALID or CHANNEL_PRIVATE.
func channelReachable(ctx context.Context, api *tg.Client, id, accessHash int64) (bool, error) {
	_, err := api.ChannelsGetChannels(ctx, []tg.InputChannelClass{
		&tg.InputChannel{ChannelID: id, AccessHash: accessHash},
	})
	switch {
	case err == nil:
		return true, nil
	case tgerr.Is(err, "CHANNEL_INVALID", "CHANNEL_PRIVATE", "CHANNEL_PUBLIC_GROUP_NA"):
		return false, nil
	default:
		return false, fmt.Errorf("telegram: check channel: %w", err)
	}
}

func updatesChats(u tg.UpdatesClass) []tg.ChatClass {
	switch v := u.(type) {
	case *tg.Updates:
		return v.Chats
	case *tg.UpdatesCombined:
		return v.Chats
	}
	return nil
}

func updatesList(u tg.UpdatesClass) []tg.UpdateClass {
	switch v := u.(type) {
	case *tg.Updates:
		return v.Updates
	case *tg.UpdatesCombined:
		return v.Updates
	case *tg.UpdateShort:
		return []tg.UpdateClass{v.Update}
	}
	return nil
}

// msgIDsByRandom maps random IDs to the message IDs Telegram assigned.
func msgIDsByRandom(u tg.UpdatesClass) map[int64]int {
	out := map[int64]int{}
	for _, up := range updatesList(u) {
		if v, ok := up.(*tg.UpdateMessageID); ok {
			out[v.RandomID] = v.ID
		}
	}
	return out
}

func randomID() int64 {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return int64(binary.LittleEndian.Uint64(b[:]))
}

func sentID(u tg.UpdatesClass, rid int64) (int, error) {
	id, ok := msgIDsByRandom(u)[rid]
	if !ok {
		return 0, errors.New("telegram: message ID missing from response")
	}
	return id, nil
}

func (s *tgStore) upload(ctx context.Context, name string, r io.Reader, size int64) (any, error) {
	up := uploader.NewUploader(s.api).WithThreads(s.threads)
	file, err := up.Upload(ctx, uploader.NewUpload(name, r, size))
	if err != nil {
		return nil, fmt.Errorf("telegram: upload: %w", err)
	}
	return file, nil
}

func (s *tgStore) sendFile(ctx context.Context, name string, file any, caption string) (int, error) {
	f, ok := file.(tg.InputFileClass)
	if !ok {
		return 0, fmt.Errorf("telegram: bad upload handle %T", file)
	}
	rid := randomID()
	upd, err := s.api.MessagesSendMedia(ctx, &tg.MessagesSendMediaRequest{
		Silent: true,
		Peer:   s.peer,
		Media: &tg.InputMediaUploadedDocument{
			ForceFile:  true,
			File:       f,
			MimeType:   "application/octet-stream",
			Attributes: []tg.DocumentAttributeClass{&tg.DocumentAttributeFilename{FileName: name}},
		},
		Message:  caption,
		RandomID: rid,
	})
	if err != nil {
		return 0, fmt.Errorf("telegram: send document: %w", err)
	}
	return sentID(upd, rid)
}

func (s *tgStore) sendText(ctx context.Context, text string) (int, error) {
	rid := randomID()
	upd, err := s.api.MessagesSendMessage(ctx, &tg.MessagesSendMessageRequest{
		Silent:    true,
		NoWebpage: true,
		Peer:      s.peer,
		Message:   text,
		RandomID:  rid,
	})
	if err != nil {
		return 0, fmt.Errorf("telegram: send message: %w", err)
	}
	return sentID(upd, rid)
}

func (s *tgStore) deleteMessages(ctx context.Context, ids []int) error {
	for len(ids) > 0 {
		n := min(len(ids), 100)
		if _, err := s.api.ChannelsDeleteMessages(ctx, &tg.ChannelsDeleteMessagesRequest{
			Channel: s.channel, ID: ids[:n],
		}); err != nil {
			return fmt.Errorf("telegram: delete messages: %w", err)
		}
		ids = ids[n:]
	}
	return nil
}

func (s *tgStore) editCaption(ctx context.Context, id int, caption string) error {
	req := &tg.MessagesEditMessageRequest{Peer: s.peer, ID: id}
	req.SetMessage(caption)
	_, err := s.api.MessagesEditMessage(ctx, req)
	if err != nil && !tgerr.Is(err, "MESSAGE_NOT_MODIFIED") {
		return fmt.Errorf("telegram: edit caption: %w", err)
	}
	return nil
}

func (s *tgStore) forward(ctx context.Context, src store, ids []int) ([]int, error) {
	from, ok := src.(*tgStore)
	if !ok {
		return nil, fmt.Errorf("telegram: cannot forward from %T", src)
	}
	rids := make([]int64, len(ids))
	for i := range rids {
		rids[i] = randomID()
	}
	upd, err := s.api.MessagesForwardMessages(ctx, &tg.MessagesForwardMessagesRequest{
		Silent:            true,
		DropAuthor:        true,
		DropMediaCaptions: true,
		FromPeer:          from.peer,
		ToPeer:            s.peer,
		ID:                ids,
		RandomID:          rids,
	})
	if err != nil {
		return nil, fmt.Errorf("telegram: forward: %w", err)
	}
	byRandom := msgIDsByRandom(upd)
	out := make([]int, len(ids))
	for i, rid := range rids {
		id, ok := byRandom[rid]
		if !ok {
			return nil, errors.New("telegram: forwarded message ID missing from response")
		}
		out[i] = id
	}
	return out, nil
}

func messagesOf(r tg.MessagesMessagesClass) []tg.MessageClass {
	if v, ok := r.(interface{ GetMessages() []tg.MessageClass }); ok {
		return v.GetMessages()
	}
	return nil
}

// getMessages fetches messages by ID. Missing IDs are absent from the map.
func (s *tgStore) getMessages(ctx context.Context, ids []int) (map[int]*tg.Message, error) {
	out := map[int]*tg.Message{}
	for len(ids) > 0 {
		n := min(len(ids), 100)
		in := make([]tg.InputMessageClass, n)
		for i, id := range ids[:n] {
			in[i] = &tg.InputMessageID{ID: id}
		}
		res, err := s.api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{Channel: s.channel, ID: in})
		if err != nil {
			return nil, fmt.Errorf("telegram: get messages: %w", err)
		}
		for _, m := range messagesOf(res) {
			if msg, ok := m.(*tg.Message); ok {
				out[msg.ID] = msg
			}
		}
		ids = ids[n:]
	}
	return out, nil
}

func messageDoc(m *tg.Message) (*tg.Document, bool) {
	media, ok := m.Media.(*tg.MessageMediaDocument)
	if !ok {
		return nil, false
	}
	doc, ok := media.Document.(*tg.Document)
	return doc, ok
}

func (s *tgStore) resolve(ctx context.Context, ids []int) ([]part, error) {
	msgs, err := s.getMessages(ctx, ids)
	if err != nil {
		return nil, err
	}
	parts := make([]part, len(ids))
	for i, id := range ids {
		m, ok := msgs[id]
		if !ok {
			return nil, fs.ErrorObjectNotFound
		}
		doc, ok := messageDoc(m)
		if !ok {
			return nil, fmt.Errorf("telegram: message %d has no document", id)
		}
		parts[i] = part{ref: doc, size: doc.Size}
	}
	return parts, nil
}

func (s *tgStore) readBlock(ctx context.Context, p part, offset int64) ([]byte, error) {
	doc := p.ref.(*tg.Document)
	inv, err := s.client.FileDC(ctx, doc.DCID)
	if err != nil {
		return nil, err
	}
	res, err := tg.NewClient(inv).UploadGetFile(ctx, &tg.UploadGetFileRequest{
		Location: &tg.InputDocumentFileLocation{
			ID:            doc.ID,
			AccessHash:    doc.AccessHash,
			FileReference: doc.FileReference,
		},
		Offset: offset,
		Limit:  blockSize,
	})
	if tgerr.Is(err, "FILE_REFERENCE_EXPIRED") {
		return nil, errRefExpired
	}
	if err != nil {
		return nil, fmt.Errorf("telegram: download: %w", err)
	}
	f, ok := res.(*tg.UploadFile)
	if !ok {
		return nil, fmt.Errorf("telegram: unexpected getFile response %T", res)
	}
	return f.Bytes, nil
}

func (s *tgStore) history(ctx context.Context, minID int) ([]rawMsg, error) {
	var out []rawMsg
	offsetID := 0
	for {
		res, err := s.api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: s.peer, OffsetID: offsetID, MinID: minID, Limit: 100,
		})
		if err != nil {
			return nil, fmt.Errorf("telegram: get history: %w", err)
		}
		msgs := messagesOf(res)
		if len(msgs) == 0 {
			return out, nil
		}
		for _, m := range msgs {
			id := m.GetID()
			if offsetID == 0 || id < offsetID {
				offsetID = id
			}
			if msg, ok := m.(*tg.Message); ok {
				out = append(out, rawMsg{ID: msg.ID, Text: msg.Message, Date: time.Unix(int64(msg.Date), 0)})
			}
		}
		if offsetID <= minID+1 {
			return out, nil
		}
	}
}

func (s *tgStore) latestIndex(ctx context.Context, hintID int) (int, []byte, error) {
	var msg *tg.Message
	if hintID != 0 {
		msgs, err := s.getMessages(ctx, []int{hintID})
		if err != nil {
			return 0, nil, err
		}
		if m := msgs[hintID]; m != nil && strings.HasPrefix(m.Message, indexTag) {
			msg = m
		}
	}
	if msg == nil {
		res, err := s.api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
			Peer:   s.peer,
			Q:      indexTag,
			Filter: &tg.InputMessagesFilterDocument{},
			Limit:  1,
		})
		if err != nil {
			return 0, nil, fmt.Errorf("telegram: search index: %w", err)
		}
		for _, m := range messagesOf(res) {
			if mm, ok := m.(*tg.Message); ok && strings.HasPrefix(mm.Message, indexTag) {
				msg = mm
			}
		}
	}
	if msg == nil {
		return 0, nil, nil
	}
	doc, ok := messageDoc(msg)
	if !ok {
		return 0, nil, errors.New("telegram: index message has no document")
	}
	var buf bytes.Buffer
	p := part{ref: doc, size: doc.Size}
	for off := int64(0); off < doc.Size; off += blockSize {
		b, err := s.readBlock(ctx, p, off)
		if err != nil {
			return 0, nil, err
		}
		buf.Write(b)
	}
	return msg.ID, buf.Bytes(), nil
}

var (
	_ account = (*tgAccount)(nil)
	_ store   = (*tgStore)(nil)
)
