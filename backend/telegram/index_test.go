package telegram

import (
	"reflect"
	"testing"
)

func msg(id int, m meta) rawMsg { return rawMsg{ID: id, Text: m.encode()} }

func TestParseMeta(t *testing.T) {
	m := meta{FileID: "a", Path: "x/y", Part: 2, ModTime: 5, Last: true, Size: 9, MD5: "h"}
	got, ok := parseMeta(m.encode())
	if !ok || got.Path != "x/y" || got.Part != 2 || !got.Last || got.Size != 9 {
		t.Fatalf("round trip failed: %+v %v", got, ok)
	}
	for _, s := range []string{
		"", "hello", "#rclone_index {}", `{"rclone":1}`,
		`{"rclone":2,"f":"a","p":"b"}`, `{"rclone":1,"op":"bogus","p":"b"}`,
		`{"rclone":1,"op":"del","p":"b"}`, // delete needs a file ID
	} {
		if _, ok := parseMeta(s); ok {
			t.Errorf("parseMeta(%q) accepted", s)
		}
	}
}

func TestReplay(t *testing.T) {
	x := newIndex()
	x.replay([]rawMsg{
		// Out of order on purpose; replay must sort.
		msg(3, meta{FileID: "big", Path: "a/b/big.bin", Part: 1, Last: true, Size: 30, MD5: "m1"}),
		msg(2, meta{FileID: "big", Path: "a/b/big.bin", Part: 0}),
		msg(4, meta{FileID: "s1", Path: "f.txt", Last: true, Size: 1}),
		{ID: 5, Text: "some unrelated post"},
		msg(6, meta{FileID: "s2", Path: "f.txt", Last: true, Size: 2}),    // newer version wins
		msg(7, meta{FileID: "inc", Path: "partial", Part: 1, Last: true}), // part 0 missing
	})
	if got := x.Files["a/b/big.bin"]; got == nil || !reflect.DeepEqual(got.Parts, []int{2, 3}) || got.MD5 != "m1" {
		t.Fatalf("big.bin wrong: %+v", got)
	}
	if got := x.Files["f.txt"]; got == nil || got.Size != 2 || got.Parts[0] != 6 {
		t.Fatalf("f.txt wrong: %+v", got)
	}
	if _, ok := x.Files["partial"]; ok {
		t.Fatal("incomplete file indexed")
	}
	if _, ok := x.Pending["inc"]; !ok {
		t.Fatal("incomplete file should stay pending")
	}
	if !x.Dirs["a"] || !x.Dirs["a/b"] || x.LastMsgID != 7 {
		t.Fatalf("dirs=%v last=%d", x.Dirs, x.LastMsgID)
	}
}

func TestReplayOps(t *testing.T) {
	x := newIndex()
	x.replay([]rawMsg{
		msg(1, meta{FileID: "a", Path: "d/a", Last: true}),
		msg(2, meta{Op: opMove, FileID: "a", From: "d/a", Path: "d/b"}),
		msg(3, meta{Op: opMove, FileID: "zzz", From: "d/b", Path: "d/c"}), // wrong file: ignored
		msg(4, meta{Op: opDirMove, From: "d", Path: "e"}),
		msg(5, meta{FileID: "n", Path: "d/new", Last: true}), // after the dir move
		msg(6, meta{Op: opMkdir, Path: "m/n"}),
		msg(7, meta{Op: opSetMtime, FileID: "a", Path: "e/b", ModTime: 42}),
		msg(8, meta{FileID: "z", Path: "z", Last: true}),
		msg(9, meta{Op: opDelete, FileID: "z", Path: "z"}),
		msg(10, meta{Op: opMkdir, Path: "tmp"}),
		msg(11, meta{Op: opRmdir, Path: "tmp"}),
	})
	files, dirs := x.list("e")
	if !reflect.DeepEqual(files, []string{"e/b"}) || len(dirs) != 0 {
		t.Fatalf("e: %v %v", files, dirs)
	}
	if x.Files["e/b"].ModTime != 42 {
		t.Fatal("mtime not applied")
	}
	if x.Files["d/new"] == nil {
		t.Fatal("file created after dir move should stay in d")
	}
	if x.Files["z"] != nil || x.Dirs["tmp"] || !x.Dirs["m/n"] || !x.Dirs["m"] {
		t.Fatalf("files=%v dirs=%v", x.Files, x.Dirs)
	}
}

func TestListAndDirs(t *testing.T) {
	x := newIndex()
	for _, p := range []string{"top.txt", "a/1", "a/2", "a/b/3"} {
		x.Files[p] = &fileEntry{}
		x.addParents(p)
	}
	x.Dirs["empty"] = true

	files, dirs := x.list("")
	if !reflect.DeepEqual(files, []string{"top.txt"}) || !reflect.DeepEqual(dirs, []string{"a", "empty"}) {
		t.Fatalf("root: %v %v", files, dirs)
	}
	files, dirs = x.list("a")
	if !reflect.DeepEqual(files, []string{"a/1", "a/2"}) || !reflect.DeepEqual(dirs, []string{"a/b"}) {
		t.Fatalf("a: %v %v", files, dirs)
	}
	if !x.dirExists("a/b") || x.dirExists("nope") || x.dirExists("top.txt") {
		t.Fatal("dirExists wrong")
	}
	if !x.hasChildren("a") || x.hasChildren("empty") {
		t.Fatal("hasChildren wrong")
	}
}

func TestCheckCaption(t *testing.T) {
	if err := checkCaption("short/path"); err != nil {
		t.Fatal(err)
	}
	long := make([]byte, 1000)
	for i := range long {
		long[i] = 'a'
	}
	if err := checkCaption(string(long)); err == nil {
		t.Fatal("expected error for long path")
	}
}
