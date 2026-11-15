// Package telegram provides an rclone backend that stores files as
// documents in private Telegram channels owned by a user account, one
// channel per bucket.
package telegram

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/telegram/auth"
	"github.com/gotd/td/tg"
	"github.com/rclone/rclone/backend/overview"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/config/obscure"
	"github.com/rclone/rclone/lib/encoder"
)

// maxChunk is the largest document a non-Premium account can upload.
const maxChunk = fs.SizeSuffix(2000 * fs.Mebi)

func init() {
	fs.Register(&fs.RegInfo{
		Name:        "telegram",
		Description: "Telegram (private channels in a user account)",
		NewFs:       NewFs,
		Config:      configure,
		Overview: &overview.BackendConfig{
			Backend:     "telegram",
			Name:        "Telegram",
			Tier:        "experimental",
			Maintainers: "rclone-telegram",
			Remote:      "telegram",
			Hashes:      []string{"MD5"},
			Precision:   1,
		},
		CommandHelp: []fs.CommandHelp{{
			Name:  "rebuild",
			Short: "Rebuild the index by replaying the whole bucket channel.",
			Long: `Replays every message in a bucket's channel to rebuild its path
index. Use it if listings look wrong. Without a bucket in the path, every
bucket is rebuilt.

    rclone backend rebuild remote:bucket`,
		}, {
			Name:  "cleanup",
			Short: "Delete data messages no file refers to.",
			Long: `Deletes parts left by failed or replaced uploads, and old index
snapshots. Unfinished uploads younger than 24h are kept, so uploads in
progress on other machines are not affected. Without a bucket in the
path, every bucket is cleaned.

    rclone backend cleanup remote:bucket`,
		}},
		Options: []fs.Option{{
			Name:      "api_id",
			Help:      "Telegram API ID.\n\nCreate one at https://my.telegram.org/apps.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:      "api_hash",
			Help:      "Telegram API hash.\n\nCreate one at https://my.telegram.org/apps.",
			Required:  true,
			Sensitive: true,
		}, {
			Name:     "channel_title",
			Help:     "Title of the private channel that lists the buckets.\n\nEach bucket is a channel titled <channel_title>/<bucket>.",
			Default:  "rclone",
			Advanced: true,
		}, {
			Name:     "chunk_size",
			Help:     "Files larger than this are split into several messages.\n\nMax 2000Mi (4000Mi with Telegram Premium).",
			Default:  maxChunk,
			Advanced: true,
		}, {
			Name:     "upload_concurrency",
			Help:     "Number of parallel part uploads per file.",
			Default:  4,
			Advanced: true,
		}, {
			Name:     "flush_interval",
			Help:     "How long to batch changes before uploading a new index snapshot.",
			Default:  fs.Duration(10 * time.Second),
			Advanced: true,
		}, {
			Name:     "refresh_interval",
			Help:     "How often to pick up changes made by other processes.\n\nSet to 0 to disable.",
			Default:  fs.Duration(time.Minute),
			Advanced: true,
		}, {
			Name:     config.ConfigEncoding,
			Help:     config.ConfigEncodingHelp,
			Advanced: true,
			Default:  encoder.Standard | encoder.EncodeInvalidUtf8,
		}, {
			Name:      "session",
			Help:      "MTProto session (set by rclone config).",
			Hide:      fs.OptionHideBoth,
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:     "channel_id",
			Help:     "ID of the channel that lists the buckets. Created automatically if empty.",
			Advanced: true,
		}, {
			Name:      "channel_access_hash",
			Help:      "Access hash of the channel that lists the buckets. Set automatically.",
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:     "index_msg_ids",
			Help:     "Message IDs of the latest index snapshot of each bucket. Set automatically.",
			Hide:     fs.OptionHideBoth,
			Advanced: true,
		}, {
			Name:      "phone",
			Help:      "Phone number used to log in (set by rclone config).",
			Hide:      fs.OptionHideBoth,
			Advanced:  true,
			Sensitive: true,
		}, {
			Name:      "phone_code_hash",
			Help:      "Login state (set by rclone config).",
			Hide:      fs.OptionHideBoth,
			Advanced:  true,
			Sensitive: true,
		}},
	})
}

// Options defines the configuration for this backend.
type Options struct {
	APIID             int                  `config:"api_id"`
	APIHash           string               `config:"api_hash"`
	ChannelTitle      string               `config:"channel_title"`
	ChunkSize         fs.SizeSuffix        `config:"chunk_size"`
	UploadConcurrency int                  `config:"upload_concurrency"`
	FlushInterval     fs.Duration          `config:"flush_interval"`
	RefreshInterval   fs.Duration          `config:"refresh_interval"`
	Enc               encoder.MultiEncoder `config:"encoding"`
	ChannelID         int64                `config:"channel_id"`
	ChannelAccessHash int64                `config:"channel_access_hash"`
}

// configure runs the interactive login: phone, code, then optional 2FA.
func configure(ctx context.Context, name string, m configmap.Mapper, in fs.ConfigIn) (*fs.ConfigOut, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	withClient := func(fn func(c *auth.Client, api *tg.Client) (*fs.ConfigOut, error)) (*fs.ConfigOut, error) {
		client, err := connect(ctx, opt, m)
		if err != nil {
			return nil, err
		}
		defer func() { _ = client.Close() }()
		return fn(client.Auth(), client.API())
	}

	switch in.State {
	case "":
		if v, _ := m.Get("session"); v != "" {
			return withClient(func(c *auth.Client, api *tg.Client) (*fs.ConfigOut, error) {
				st, err := c.Status(ctx)
				if err == nil && st.Authorized {
					return fs.ConfigConfirm("relogin", false, "config_relogin", "Already logged in. Log in again?")
				}
				m.Set("session", "")
				return fs.ConfigGoto("phone")
			})
		}
		return fs.ConfigGoto("phone")
	case "relogin":
		if in.Result != "true" {
			return nil, nil
		}
		m.Set("session", "")
		return fs.ConfigGoto("phone")
	case "phone":
		return fs.ConfigInput("phone_done", "config_phone", "Phone number in international format, e.g. +33612345678.")
	case "phone_done":
		phone := strings.TrimSpace(in.Result)
		return withClient(func(c *auth.Client, api *tg.Client) (*fs.ConfigOut, error) {
			sent, err := c.SendCode(ctx, phone, auth.SendCodeOptions{})
			if err != nil {
				return fs.ConfigError("phone", fmt.Sprintf("Sending code failed: %v", err))
			}
			switch s := sent.(type) {
			case *tg.AuthSentCode:
				m.Set("phone", phone)
				m.Set("phone_code_hash", s.PhoneCodeHash)
				return fs.ConfigInput("code_done", "config_code", "Login code sent by Telegram.")
			case *tg.AuthSentCodeSuccess:
				return finishLogin(ctx, api, opt, m)
			default:
				return fs.ConfigError("phone", fmt.Sprintf("Unexpected response %T", sent))
			}
		})
	case "code_done":
		phone, _ := m.Get("phone")
		codeHash, _ := m.Get("phone_code_hash")
		return withClient(func(c *auth.Client, api *tg.Client) (*fs.ConfigOut, error) {
			_, err := c.SignIn(ctx, phone, strings.TrimSpace(in.Result), codeHash)
			if errors.Is(err, auth.ErrPasswordAuthNeeded) {
				return fs.ConfigPassword("password_done", "config_password", "Two-step verification password.")
			}
			if err != nil {
				return fs.ConfigError("phone", fmt.Sprintf("Sign in failed: %v", err))
			}
			return finishLogin(ctx, api, opt, m)
		})
	case "password_done":
		pw, err := obscure.Reveal(in.Result)
		if err != nil {
			pw = in.Result
		}
		return withClient(func(c *auth.Client, api *tg.Client) (*fs.ConfigOut, error) {
			if _, err := c.Password(ctx, pw); err != nil {
				return fs.ConfigError("password_retry", fmt.Sprintf("Password rejected: %v", err))
			}
			return finishLogin(ctx, api, opt, m)
		})
	case "password_retry":
		return fs.ConfigPassword("password_done", "config_password", "Two-step verification password.")
	}
	return nil, fmt.Errorf("telegram: unknown config state %q", in.State)
}

// finishLogin keeps the stored registry channel only if the logged-in
// account can reach it. Channel access hashes are per account, so after
// switching accounts the old settings are cleared and a new registry
// channel is created on first use.
func finishLogin(ctx context.Context, api *tg.Client, opt *Options, m configmap.Mapper) (*fs.ConfigOut, error) {
	m.Set("phone_code_hash", "")
	if opt.ChannelID == 0 {
		return nil, nil
	}
	ok, err := channelReachable(ctx, api, opt.ChannelID, opt.ChannelAccessHash)
	if err != nil {
		return nil, err
	}
	if !ok {
		fs.Logf(nil, "telegram: this account cannot access channel %d: a new channel will be created on first use", opt.ChannelID)
		resetChannel(m)
	}
	return nil, nil
}

// resetChannel forgets the registry channel and the index hints.
func resetChannel(m configmap.Mapper) {
	m.Set("channel_id", "0")
	m.Set("channel_access_hash", "0")
	m.Set("index_msg_ids", "")
}
