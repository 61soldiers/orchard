package m4a

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/itouakirai/mp4ff/mp4"
)

func remux(t *testing.T, fixture string, tags Tags) []byte {
	t.Helper()
	in, err := os.Open(filepath.Join("testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	var out bytes.Buffer
	if err := Progressive(in, &out, t.TempDir(), tags); err != nil {
		t.Fatalf("Progressive: %v", err)
	}
	return out.Bytes()
}

func sampleCount(t *testing.T, b []byte) (uint32, uint64) {
	t.Helper()
	f, err := mp4.DecodeFile(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("output does not parse: %v", err)
	}
	if f.IsFragmented() {
		t.Fatal("output is still fragmented")
	}
	if f.Moov == nil || len(f.Moov.Traks) != 1 {
		t.Fatal("output has no single-track moov")
	}
	stbl := f.Moov.Traks[0].Mdia.Minf.Stbl
	if stbl.Stsz == nil || stbl.Stsc == nil || stbl.Stco == nil || stbl.Stts == nil {
		t.Fatal("sample table is incomplete")
	}
	return stbl.Stsz.SampleNumber, f.Moov.Traks[0].Mdia.Mdhd.Duration
}

func TestProgressiveParsesAndKeepsEverySample(t *testing.T) {
	for _, fixture := range []string{"frag_aac.mp4", "frag_alac.mp4"} {
		t.Run(fixture, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", fixture))
			if err != nil {
				t.Fatal(err)
			}
			src, err := mp4.DecodeFile(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			var wantSamples int
			var wantDur uint64
			for _, seg := range src.Segments {
				for _, fr := range seg.Fragments {
					ss, err := fr.GetFullSamples(src.Moov.Mvex.Trex)
					if err != nil {
						t.Fatal(err)
					}
					wantSamples += len(ss)
					for _, s := range ss {
						wantDur += uint64(s.Dur)
					}
				}
			}

			n, dur := sampleCount(t, remux(t, fixture, Tags{Title: "x"}))
			if int(n) != wantSamples {
				t.Errorf("samples = %d, want %d", n, wantSamples)
			}
			if dur != wantDur {
				t.Errorf("duration = %d, want %d", dur, wantDur)
			}
		})
	}
}

func TestProgressiveWritesTagsAndCover(t *testing.T) {
	cover, err := os.ReadFile(filepath.Join("testdata", "cover.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	b := remux(t, "frag_aac.mp4", Tags{
		Title: "Täg Title", Artist: "An Artist", Album: "The Album", AlbumArtist: "AA",
		Composer: "Comp", Date: "2021-03-19", Genre: "Pop", Track: 3, Disc: 2, Cover: cover,
	})
	for _, want := range []string{"\xa9nam", "Täg Title", "\xa9ART", "An Artist", "\xa9alb", "The Album",
		"aART", "\xa9wrt", "\xa9day", "2021-03-19", "\xa9gen", "trkn", "disk", "covr", "mdir", "appl"} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("output is missing %q", want)
		}
	}
	if !bytes.Contains(b, cover) {
		t.Error("cover bytes are not embedded")
	}
}

func TestProgressiveRejectsEmptyAudio(t *testing.T) {
	// A bare ftyp+moov and no fragments.
	in, err := os.ReadFile(filepath.Join("testdata", "frag_aac.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	f, err := mp4.DecodeFile(bytes.NewReader(in))
	if err != nil {
		t.Fatal(err)
	}
	var head bytes.Buffer
	if err := f.Init.Encode(&head); err != nil {
		t.Fatal(err)
	}
	if err := Progressive(&head, &bytes.Buffer{}, t.TempDir(), Tags{}); err == nil {
		t.Fatal("expected an error for a file with no samples")
	}
}

// With ffmpeg around, prove the result is a real, decodable file whose audio
// packets are byte-identical to the source's.
func TestProgressiveMatchesSourceAudioPackets(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed")
	}
	cover, _ := os.ReadFile(filepath.Join("testdata", "cover.jpg"))
	for _, fixture := range []string{"frag_aac.mp4", "frag_alac.mp4"} {
		t.Run(fixture, func(t *testing.T) {
			out := filepath.Join(t.TempDir(), "out.m4a")
			if err := os.WriteFile(out, remux(t, fixture, Tags{Title: "T", Artist: "A", Track: 1, Cover: cover}), 0o600); err != nil {
				t.Fatal(err)
			}
			sum := func(path string) string {
				b, err := exec.Command("ffmpeg", "-v", "error", "-i", path, "-map", "0:a", "-c", "copy", "-f", "md5", "-").CombinedOutput()
				if err != nil {
					t.Fatalf("ffmpeg %s: %v\n%s", path, err, b)
				}
				return strings.TrimSpace(string(b))
			}
			if got, want := sum(out), sum(filepath.Join("testdata", fixture)); got != want {
				t.Errorf("audio packets differ: %s != %s", got, want)
			}
			if b, err := exec.Command("ffmpeg", "-v", "error", "-i", out, "-map", "0:a", "-f", "null", "-").CombinedOutput(); err != nil || len(b) != 0 {
				t.Errorf("output does not decode cleanly: %v\n%s", err, b)
			}
			if _, err := exec.LookPath("ffprobe"); err == nil {
				b, _ := exec.Command("ffprobe", "-v", "error", "-show_entries", "format_tags=title,artist,track", "-of", "default=nw=1", out).Output()
				for _, want := range []string{"TAG:title=T", "TAG:artist=A", "TAG:track=1"} {
					if !strings.Contains(string(b), want) {
						t.Errorf("ffprobe tags missing %q:\n%s", want, b)
					}
				}
			}
		})
	}
}
