package telegram

import (
	"context"
	"errors"
	"io"
)

// blockSize is the download request size. Offsets aligned to it satisfy
// every Telegram constraint on upload.getFile offset/limit.
const blockSize = 1 << 20

// captionLimit is the maximum caption length for non-Premium accounts.
const captionLimit = 1024

// errRefExpired means a part's file reference expired: resolve it again.
var errRefExpired = errors.New("telegram: file reference expired")

// part is a resolved data message ready for download.
type part struct {
	ref  any // store-specific handle
	size int64
}

// channelRef identifies a channel the logged-in account can use.
type channelRef struct {
	ID   int64 `json:"id"`
	Hash int64 `json:"hash"`
}

// account is one logged-in Telegram user that owns the registry channel
// and one channel per bucket. The real implementation is tgAccount; tests
// use an in-memory fake.
type account interface {
	// registry returns the channel that records which buckets exist.
	registry() store
	// channel returns the store of a bucket channel.
	channel(ref channelRef) store
	// createChannel creates a private channel.
	createChannel(ctx context.Context, title string) (channelRef, error)
	// setTitle renames a channel.
	setTitle(ctx context.Context, ref channelRef, title string) error
	// deleteChannel deletes a channel and every message in it.
	deleteChannel(ctx context.Context, ref channelRef) error
	// close releases the connection.
	close() error
}

// store is everything the backend needs from one Telegram channel.
type store interface {
	// upload sends r (size -1 if unknown) and returns a handle for sendFile.
	upload(ctx context.Context, name string, r io.Reader, size int64) (any, error)
	// sendFile posts an uploaded file as a document with a caption.
	sendFile(ctx context.Context, name string, file any, caption string) (int, error)
	// sendText posts a text message.
	sendText(ctx context.Context, text string) (int, error)
	// deleteMessages deletes messages; missing IDs are ignored.
	deleteMessages(ctx context.Context, ids []int) error
	// editCaption replaces a message caption.
	editCaption(ctx context.Context, id int, caption string) error
	// forward copies messages from src (this channel or another channel
	// of the same account) into this channel, without their captions.
	forward(ctx context.Context, src store, ids []int) ([]int, error)
	// resolve looks up data messages for download.
	resolve(ctx context.Context, ids []int) ([]part, error)
	// readBlock reads up to blockSize bytes at a blockSize-aligned offset.
	readBlock(ctx context.Context, p part, offset int64) ([]byte, error)
	// history returns all messages with ID > minID.
	history(ctx context.Context, minID int) ([]rawMsg, error)
	// latestIndex returns the newest index snapshot, trying hintID first.
	// It returns id 0 if there is none.
	latestIndex(ctx context.Context, hintID int) (id int, data []byte, err error)
}
