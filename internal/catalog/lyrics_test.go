package catalog

import "testing"

const syllableDoc = `<tt xmlns="http://www.w3.org/ns/ttml" xmlns:itunes="http://music.apple.com/lyric-ttml-internal" xmlns:ttm="http://www.w3.org/ns/ttml#metadata" itunes:timing="Word" xml:lang="en"><head/><body dur="1:00.000"><div begin="8.789" end="20.000">` +
	`<p begin="8.789" end="12.769" itunes:key="L1"><span begin="8.789" end="9.100">I'm</span> <span begin="9.100" end="9.500">a</span> <span begin="9.500" end="10.000">wa</span><span begin="10.000" end="10.500">ter</span> <span ttm:role="x-bg"><span begin="10.200" end="10.600">(oh</span> <span begin="10.600" end="11.000">oh)</span></span><span ttm:role="x-translation">ignored</span></p>` +
	`<p begin="1:02.500" end="1:04.000" itunes:key="L2"><span begin="1:02.500" end="1:03.000">Again</span></p>` +
	`</div></body></tt>`

func TestWordLevelLRC(t *testing.T) {
	lrc, level, err := ttmlToLRC(syllableDoc)
	if err != nil || level != SyncWord {
		t.Fatalf("level=%q err=%v", level, err)
	}
	want := "[00:08.789]<00:08.789>I'm <00:09.100>a <00:09.500>wa<00:10.000>ter<00:10.500>\n" +
		"[00:10.200]<00:10.200>(oh <00:10.600>oh)<00:11.000>\n" +
		"[01:02.500]<01:02.500>Again<01:03.000>\n"
	if lrc != want {
		t.Fatalf("got:\n%s\nwant:\n%s", lrc, want)
	}
}

func TestLineDocStaysLineLevel(t *testing.T) {
	doc := `<tt xmlns:itunes="x" itunes:timing="Line"><body><div><p begin="8.789">Hello</p></div></body></tt>`
	lrc, level, err := ttmlToLRC(doc)
	if err != nil || level != SyncLine || lrc != "[00:08.789]Hello\n" {
		t.Fatalf("lrc=%q level=%q err=%v", lrc, level, err)
	}
}
