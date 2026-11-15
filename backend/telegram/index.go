package telegram

import (
	"encoding/json"
	"path"
	"sort"
	"strings"
	"time"
)

// metaTag identifies captions and texts written by this backend.
const metaTag = "rclone"

// indexTag is the caption prefix of index snapshot messages.
const indexTag = "#rclone_index"

// Operation types stored in op messages. An empty op is a data part.
const (
	opDelete   = "del"   // Path, FileID
	opMove     = "mv"    // From -> Path, FileID
	opDirMove  = "dmv"   // From -> Path
	opMkdir    = "mkdir" // Path
	opRmdir    = "rmdir" // Path
	opSetMtime = "mtime" // Path, FileID, ModTime
)

// meta is stored as JSON in the caption of every data part and in the
// text of every op message.
//
// The channel is an append-only log: replaying every message in ID order
// rebuilds the exact index. Snapshots only make loading faster.
type meta struct {
	Tag     int    `json:"rclone"`         // always 1
	Op      string `json:"op,omitempty"`   // "" for data parts
	FileID  string `json:"f,omitempty"`    // random per-upload ID grouping parts
	Path    string `json:"p"`              // full path from channel root
	From    string `json:"from,omitempty"` // source path for moves
	Part    int    `json:"i,omitempty"`    // part number, 0-based
	ModTime int64  `json:"m,omitempty"`    // unix nanoseconds
	Last    bool   `json:"last,omitempty"` // set on the final part
	Size    int64  `json:"s,omitempty"`    // total size, final part only
	MD5     string `json:"h,omitempty"`    // hex MD5 of the file, final part only
}

func (m *meta) encode() string {
	m.Tag = 1
	b, _ := json.Marshal(m)
	return string(b)
}

func parseMeta(s string) (*meta, bool) {
	if !strings.HasPrefix(s, "{\""+metaTag+"\"") {
		return nil, false
	}
	var m meta
	if err := json.Unmarshal([]byte(s), &m); err != nil || m.Tag != 1 {
		return nil, false
	}
	switch m.Op {
	case "":
		if m.FileID == "" || m.Path == "" {
			return nil, false
		}
	case opDelete, opMove, opSetMtime:
		if m.FileID == "" || m.Path == "" {
			return nil, false
		}
	case opDirMove, opMkdir, opRmdir:
		if m.Path == "" {
			return nil, false
		}
	default:
		return nil, false
	}
	return &m, true
}

// fileEntry is one file in the index.
type fileEntry struct {
	FileID  string `json:"f"`
	Size    int64  `json:"s"`
	ModTime int64  `json:"m"`
	MD5     string `json:"h,omitempty"`
	Parts   []int  `json:"p"` // message IDs in part order
}

// index is the state of the channel log up to LastMsgID.
type index struct {
	Version   int                   `json:"v"`
	LastMsgID int                   `json:"last"`
	Files     map[string]*fileEntry `json:"files"`
	Dirs      map[string]bool       `json:"dirs"`
	// Pending holds parts of uploads whose final part has not been seen
	// yet: FileID -> part number -> message ID.
	Pending map[string]map[int]int `json:"pending,omitempty"`
}

func newIndex() *index {
	return (&index{Version: 1}).init()
}

func (x *index) init() *index {
	if x.Files == nil {
		x.Files = map[string]*fileEntry{}
	}
	if x.Dirs == nil {
		x.Dirs = map[string]bool{}
	}
	if x.Pending == nil {
		x.Pending = map[string]map[int]int{}
	}
	return x
}

// clone returns a deep copy.
func (x *index) clone() *index {
	y := &index{Version: x.Version, LastMsgID: x.LastMsgID}
	y.init()
	for p, e := range x.Files {
		c := *e
		c.Parts = append([]int(nil), e.Parts...)
		y.Files[p] = &c
	}
	for d := range x.Dirs {
		y.Dirs[d] = true
	}
	for f, parts := range x.Pending {
		c := make(map[int]int, len(parts))
		for k, v := range parts {
			c[k] = v
		}
		y.Pending[f] = c
	}
	return y
}

// addParents marks every ancestor directory of p as existing.
func (x *index) addParents(p string) {
	for d := path.Dir(p); d != "." && d != "/" && d != ""; d = path.Dir(d) {
		x.Dirs[d] = true
	}
}

// dirExists reports whether d is the root, an explicit dir, or a prefix
// of any file or dir.
func (x *index) dirExists(d string) bool {
	if d == "" || x.Dirs[d] {
		return true
	}
	if _, isFile := x.Files[d]; isFile {
		return false
	}
	return x.hasChildren(d)
}

// hasChildren reports whether anything lives below d.
func (x *index) hasChildren(d string) bool {
	if d == "" {
		return len(x.Files) > 0 || len(x.Dirs) > 0
	}
	prefix := d + "/"
	for p := range x.Files {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	for p := range x.Dirs {
		if strings.HasPrefix(p, prefix) {
			return true
		}
	}
	return false
}

// list returns the direct child file paths and dir paths of d (full paths).
func (x *index) list(d string) (files, dirs []string) {
	prefix := ""
	if d != "" {
		prefix = d + "/"
	}
	seenDir := map[string]bool{}
	add := func(p string, isFile bool) {
		if !strings.HasPrefix(p, prefix) || p == d {
			return
		}
		rest := p[len(prefix):]
		if i := strings.IndexByte(rest, '/'); i >= 0 {
			seenDir[prefix+rest[:i]] = true
		} else if isFile {
			files = append(files, p)
		} else {
			seenDir[p] = true
		}
	}
	for p := range x.Files {
		add(p, true)
	}
	for p := range x.Dirs {
		add(p, false)
	}
	for p := range seenDir {
		dirs = append(dirs, p)
	}
	sort.Strings(files)
	sort.Strings(dirs)
	return files, dirs
}

// moveDir moves everything under from to to.
func (x *index) moveDir(from, to string) {
	prefix := from + "/"
	for p, e := range x.Files {
		if strings.HasPrefix(p, prefix) {
			delete(x.Files, p)
			x.Files[to+p[len(from):]] = e
		}
	}
	for d := range x.Dirs {
		if d == from || strings.HasPrefix(d, prefix) {
			delete(x.Dirs, d)
			x.Dirs[to+d[len(from):]] = true
		}
	}
	x.Dirs[to] = true
	x.addParents(to)
}

// rawMsg is the minimal message view needed for replay.
type rawMsg struct {
	ID   int
	Text string
	Date time.Time
}

// replay applies messages newer than LastMsgID in ascending ID order.
func (x *index) replay(msgs []rawMsg) {
	sort.Slice(msgs, func(i, j int) bool { return msgs[i].ID < msgs[j].ID })
	for _, msg := range msgs {
		if msg.ID <= x.LastMsgID {
			continue
		}
		x.LastMsgID = msg.ID
		if m, ok := parseMeta(msg.Text); ok {
			x.apply(msg.ID, m)
		}
	}
}

func (x *index) apply(id int, m *meta) {
	switch m.Op {
	case "":
		parts := x.Pending[m.FileID]
		if parts == nil {
			parts = map[int]int{}
			x.Pending[m.FileID] = parts
		}
		parts[m.Part] = id
		if !m.Last {
			return
		}
		ids := make([]int, m.Part+1)
		for i := range ids {
			pid, ok := parts[i]
			if !ok {
				return // incomplete: wait (or let cleanup drop it)
			}
			ids[i] = pid
		}
		delete(x.Pending, m.FileID)
		x.Files[m.Path] = &fileEntry{FileID: m.FileID, Size: m.Size, ModTime: m.ModTime, MD5: m.MD5, Parts: ids}
		x.addParents(m.Path)
	case opDelete:
		if e := x.Files[m.Path]; e != nil && e.FileID == m.FileID {
			delete(x.Files, m.Path)
		}
		delete(x.Pending, m.FileID)
	case opMove:
		if e := x.Files[m.From]; e != nil && e.FileID == m.FileID {
			delete(x.Files, m.From)
			x.Files[m.Path] = e
			x.addParents(m.Path)
		}
	case opDirMove:
		x.moveDir(m.From, m.Path)
	case opMkdir:
		x.Dirs[m.Path] = true
		x.addParents(m.Path)
	case opRmdir:
		delete(x.Dirs, m.Path)
	case opSetMtime:
		if e := x.Files[m.Path]; e != nil && e.FileID == m.FileID {
			e.ModTime = m.ModTime
		}
	}
}
