package engine

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// goldenCases mirrors testdata/gen_golden.py: outputs of the original Python
// engine for seeded random inputs. The Go port must reproduce every one.
type diffCases struct {
	Mask []struct {
		S      string   `json:"s"`
		Ass    bool     `json:"ass"`
		Ent    bool     `json:"ent"`
		Masked string   `json:"masked"`
		Tags   []string `json:"tags"`
	} `json:"mask"`
	Segment []struct {
		Lines   []string `json:"lines"`
		Dialect string   `json:"dialect"`
		Segs    []struct {
			Text   string   `json:"text"`
			Tags   []string `json:"tags"`
			Dashed bool     `json:"dashed"`
			Inside bool     `json:"inside"`
		} `json:"segs"`
	} `json:"segment"`
	Wrapcjk []struct {
		S      string   `json:"s"`
		Lim    float64  `json:"lim"`
		Ml     int      `json:"ml"`
		Result []string `json:"result"`
	} `json:"wrapcjk"`
	Wraplatin []struct {
		S      string   `json:"s"`
		Lim    float64  `json:"lim"`
		Ml     int      `json:"ml"`
		Result []string `json:"result"`
	} `json:"wraplatin"`
	Rebuild []struct {
		Lines   []string `json:"lines"`
		Dialect string   `json:"dialect"`
		Outs    []string `json:"outs"`
		Lang    string   `json:"lang"`
		Width   float64  `json:"width"`
		Ml      int      `json:"ml"`
		Result  []string `json:"result"`
	} `json:"rebuild"`
	Numbered []struct {
		Out    string   `json:"out"`
		N      int      `json:"n"`
		Result []string `json:"result"`
		Error  string   `json:"error"`
	} `json:"numbered"`
	Srt []struct {
		Raw  string `json:"raw"`
		Cues []struct {
			I int      `json:"i"`
			S string   `json:"s"`
			E string   `json:"e"`
			R string   `json:"r"`
			L []string `json:"l"`
		} `json:"cues"`
	} `json:"srt"`
	Docs []struct {
		Fmt       string `json:"fmt"`
		Raw       string `json:"raw"`
		Roundtrip string `json:"roundtrip"`
		Echo      string `json:"echo"`
		EchoES    string `json:"echo_es"`
		Ncues     int    `json:"ncues"`
		Error     string `json:"error"`
	} `json:"docs"`
	Placeholders []struct {
		Src   string `json:"src"`
		Tr    string `json:"tr"`
		Match bool   `json:"match"`
		Needs bool   `json:"needs"`
	} `json:"placeholders"`
	Wrapform []struct {
		S      string   `json:"s"`
		Tags   []string `json:"tags"`
		Result string   `json:"result"`
		Back   string   `json:"back"`
	} `json:"wrapform"`
}

func nz(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func TestPythonGolden(t *testing.T) {
	file, err := os.Open(filepath.Join("testdata", "python_golden.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	unzipped, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	var c diffCases
	if err := json.NewDecoder(unzipped).Decode(&c); err != nil {
		t.Fatal(err)
	}
	bad := map[string]int{}
	report := func(kind string, format string, args ...any) {
		bad[kind]++
		if bad[kind] <= 4 {
			t.Errorf("[%s] %s", kind, fmt.Sprintf(format, args...))
		}
	}

	for _, m := range c.Mask {
		masked, tags := MaskTags(m.S, m.Ass, m.Ent)
		if masked != m.Masked || !reflect.DeepEqual(nz(tags), nz(m.Tags)) {
			report("mask", "%q ass=%v ent=%v\n go: %q %q\n py: %q %q", m.S, m.Ass, m.Ent, masked, tags, m.Masked, m.Tags)
		}
	}
	for _, s := range c.Segment {
		cue := Cue{Index: 1, Start: "a", End: "b", Lines: s.Lines, Dialect: s.Dialect}
		segs := SegmentCue(cue, 0)
		ok := len(segs) == len(s.Segs)
		for i := 0; ok && i < len(segs); i++ {
			p := s.Segs[i]
			ok = segs[i].Text == p.Text && reflect.DeepEqual(nz(segs[i].Tags), nz(p.Tags)) &&
				segs[i].Dashed == p.Dashed && segs[i].DashInside == p.Inside
		}
		if !ok {
			report("segment", "%q %q\n go: %+v\n py: %+v", s.Dialect, s.Lines, segs, s.Segs)
		}
	}
	for _, w := range c.Wrapcjk {
		if got := WrapCJK(w.S, w.Lim, w.Ml); !reflect.DeepEqual(got, w.Result) {
			report("wrapcjk", "%q lim=%v ml=%d\n go: %q\n py: %q", w.S, w.Lim, w.Ml, got, w.Result)
		}
	}
	for _, w := range c.Wraplatin {
		if got := WrapLatin(w.S, int(w.Lim), w.Ml); !reflect.DeepEqual(got, w.Result) {
			report("wraplatin", "%q lim=%v ml=%d\n go: %q\n py: %q", w.S, w.Lim, w.Ml, got, w.Result)
		}
	}
	for _, r := range c.Rebuild {
		cue := Cue{Index: 1, Start: "a", End: "b", Lines: r.Lines, Dialect: r.Dialect}
		segs := SegmentCue(cue, 0)
		got, err := RebuildCues([]Cue{cue}, segs, r.Outs, r.Lang, r.Width, r.Ml)
		if err != nil || !reflect.DeepEqual(nz(got[0].Lines), nz(r.Result)) {
			report("rebuild", "%q %q outs=%q lang=%s w=%v ml=%d\n go: %q %v\n py: %q", r.Dialect, r.Lines, r.Outs, r.Lang, r.Width, r.Ml, got[0].Lines, err, r.Result)
		}
	}
	for _, n := range c.Numbered {
		got, err := ParseNumbered(n.Out, n.N)
		switch {
		case n.Error != "":
			if err == nil || err.Error() != n.Error {
				report("numbered", "%q n=%d\n go: %v %v\n py err: %s", n.Out, n.N, got, err, n.Error)
			}
		case err != nil || !reflect.DeepEqual(got, n.Result):
			report("numbered", "%q n=%d\n go: %q %v\n py: %q", n.Out, n.N, got, err, n.Result)
		}
	}
	for _, s := range c.Srt {
		cues := ParseSRT(s.Raw)
		ok := len(cues) == len(s.Cues)
		for i := 0; ok && i < len(cues); i++ {
			p := s.Cues[i]
			ok = cues[i].Index == p.I && cues[i].Start == p.S && cues[i].End == p.E && cues[i].Rest == p.R &&
				reflect.DeepEqual(nz(cues[i].Lines), nz(p.L))
		}
		if !ok {
			report("srt", "%q\n go: %+v\n py: %+v", s.Raw, cues, s.Cues)
		}
	}
	for _, d := range c.Docs {
		doc, err := ParseSubtitle([]byte(d.Raw), d.Fmt, "")
		if d.Error != "" {
			if err == nil || err.Error() != d.Error {
				report("docs-error", "%s %q\n go: %v\n py: %s", d.Fmt, d.Raw, err, d.Error)
			}
			continue
		}
		if err != nil {
			report("docs", "%s %q\n go error: %v", d.Fmt, d.Raw, err)
			continue
		}
		rt, _ := doc.ToBytes()
		if string(rt) != d.Roundtrip || len(doc.Cues) != d.Ncues {
			report("docs-roundtrip", "%s %q\n go: %q (%d cues)\n py: %q (%d)", d.Fmt, d.Raw, rt, len(doc.Cues), d.Roundtrip, d.Ncues)
		}
		out, _, err := Translate(context.Background(), []byte(d.Raw), d.Fmt, "zh-TW", Config{Provider: "echo", Width: 6, MaxLines: 2}, nil)
		if err != nil || string(out) != d.Echo {
			report("docs-echo", "%s %q\n go: %q %v\n py: %q", d.Fmt, d.Raw, out, err, d.Echo)
		}
		out, _, err = Translate(context.Background(), []byte(d.Raw), d.Fmt, "es", Config{Provider: "echo", Width: 6, MaxLines: 2}, nil)
		if err != nil || string(out) != d.EchoES {
			report("docs-echo-es", "%s %q\n go: %q %v\n py: %q", d.Fmt, d.Raw, out, err, d.EchoES)
		}
	}
	for _, p := range c.Placeholders {
		if PlaceholdersMatch(p.Src, p.Tr) != p.Match || NeedsTranslation(Segment{Text: p.Tr}) != p.Needs {
			report("placeholders", "%q %q go=%v py=%v needs go=%v py=%v", p.Src, p.Tr, PlaceholdersMatch(p.Src, p.Tr), p.Match, NeedsTranslation(Segment{Text: p.Tr}), p.Needs)
		}
	}
	for _, w := range c.Wrapform {
		wf := ToWrapForm(w.S, w.Tags)
		if wf != w.Result || FromWrapForm(wf, w.Tags, true) != w.Back {
			report("wrapform", "%q %q\n go: %q %q\n py: %q %q", w.S, w.Tags, wf, FromWrapForm(wf, w.Tags, true), w.Result, w.Back)
		}
	}
	total := len(c.Mask) + len(c.Segment) + len(c.Wrapcjk) + len(c.Wraplatin) + len(c.Rebuild) +
		len(c.Numbered) + len(c.Srt) + len(c.Docs) + len(c.Placeholders) + len(c.Wrapform)
	t.Logf("%d golden cases, mismatches: %v", total, bad)
}
