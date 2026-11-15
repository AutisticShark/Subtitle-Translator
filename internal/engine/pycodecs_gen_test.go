package engine

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/text/encoding/htmlindex"
)

// TestDumpWHATWGForPyCodecs writes the WHATWG decoder output that
// testdata/gen_pycodecs.py compares against Python when regenerating
// pycodecs_data.go. It only runs with PYCODECS_DUMP_DIR set:
//
//	PYCODECS_DUMP_DIR=/tmp/dump go test -run TestDumpWHATWGForPyCodecs ./internal/engine
//	python -I internal/engine/testdata/gen_pycodecs.py /tmp/dump internal/engine/pycodecs_data.go
//	gofmt -w internal/engine/pycodecs_data.go
func TestDumpWHATWGForPyCodecs(t *testing.T) {
	dir := os.Getenv("PYCODECS_DUMP_DIR")
	if dir == "" {
		t.Skip("set PYCODECS_DUMP_DIR to regenerate the codec tables")
	}
	for _, label := range []string{"gbk", "big5", "shift_jis", "euc-kr", "euc-jp", "gb18030"} {
		enc, err := htmlindex.Get(label)
		if err != nil {
			t.Fatal(err)
		}
		f, err := os.Create(filepath.Join(dir, "whatwg_"+label+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		w := bufio.NewWriter(f)
		decoder := enc.NewDecoder()
		emit := func(in ...byte) {
			if out, err := decoder.Bytes(in); err != nil {
				fmt.Fprintf(w, "%s\tERR\n", hex.EncodeToString(in))
			} else {
				fmt.Fprintf(w, "%s\t%s\n", hex.EncodeToString(in), hex.EncodeToString(out))
			}
		}
		for a := 0x80; a < 0x100; a++ {
			emit(byte(a))
			for b := 0; b < 0x100; b++ {
				emit(byte(a), byte(b))
			}
		}
		switch label {
		case "euc-jp":
			for b := 0; b < 0x100; b++ {
				for c := 0; c < 0x100; c++ {
					emit(0x8f, byte(b), byte(c))
				}
			}
		case "gb18030":
			for a := 0x80; a < 0x100; a++ {
				for b := 0x30; b <= 0x39; b++ {
					for c := 0x81; c <= 0xfe; c++ {
						for d := 0x30; d <= 0x39; d++ {
							emit(byte(a), byte(b), byte(c), byte(d))
						}
					}
				}
			}
		}
		if err := w.Flush(); err != nil {
			t.Fatal(err)
		}
		_ = f.Close()
	}
}
