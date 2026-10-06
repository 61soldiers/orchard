// Package m4a turns the decrypted fragmented MP4 that internal/stream produces
// into a progressive .m4a: one moov with a full sample table in front of one
// mdat, plus iTunes-style tags and cover art. It replaces the ffmpeg
// `-c copy` pass the download pipeline used to shell out to, so Orchard has no
// external program to ship. Audio bytes are copied, never re-encoded.
package m4a

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/Eyevinn/mp4ff/bits"
	"github.com/itouakirai/mp4ff/mp4"
)

// Tags is the metadata written into the file. Empty fields are left out.
type Tags struct {
	Title       string
	Artist      string
	Album       string
	AlbumArtist string
	Composer    string
	Date        string
	Genre       string
	Track       int
	Disc        int
	// Cover is a JPEG or PNG image, embedded as the front cover when non-empty.
	Cover []byte
}

// fragment records where one moof's samples sit in the spool file.
type fragment struct {
	sampleDesc uint32
	sizes      []uint32
	durs       []uint32
	firstDelta uint64 // offset of the fragment's data in the spool
}

// Progressive reads a fragmented MP4 (ftyp, moov, then moof/mdat pairs) from in
// and writes the progressive equivalent to out. spoolDir is where the sample
// data waits while the sample table is built; it should be on the same
// filesystem as the final output, and nothing is left behind.
func Progressive(in io.Reader, out io.Writer, spoolDir string, tags Tags) error {
	r := bufio.NewReaderSize(in, 1<<20)

	init, offset, err := readInit(r)
	if err != nil {
		return fmt.Errorf("read init segment: %w", err)
	}
	if len(init.Moov.Traks) != 1 {
		return fmt.Errorf("expected exactly 1 track, got %d", len(init.Moov.Traks))
	}
	trak := init.Moov.Traks[0]
	var trex *mp4.TrexBox
	if init.Moov.Mvex != nil {
		trex = init.Moov.Mvex.Trex
	}

	spool, err := os.CreateTemp(spoolDir, "m4a-spool-*")
	if err != nil {
		return err
	}
	defer os.Remove(spool.Name())
	defer spool.Close()
	sw := bufio.NewWriterSize(spool, 1<<20)

	var (
		frags     []fragment
		written   uint64
		sampleNum uint64
	)
	for {
		frag, next, err := readFragment(r, offset)
		if err != nil {
			return err
		}
		if frag == nil {
			break
		}
		offset = next

		samples, err := frag.GetFullSamples(trex)
		if err != nil {
			return fmt.Errorf("read fragment samples: %w", err)
		}
		if len(samples) == 0 {
			continue
		}
		f := fragment{sampleDesc: 1, firstDelta: written}
		if tfhd := frag.Moof.Traf.Tfhd; tfhd.HasSampleDescriptionIndex() {
			f.sampleDesc = tfhd.SampleDescriptionIndex
		} else if trex != nil && trex.DefaultSampleDescriptionIndex != 0 {
			f.sampleDesc = trex.DefaultSampleDescriptionIndex
		}
		for _, s := range samples {
			if s.CompositionTimeOffset != 0 {
				return errors.New("composition time offsets are not supported (audio only)")
			}
			n, err := sw.Write(s.Data)
			if err != nil {
				return err
			}
			written += uint64(n)
			f.sizes = append(f.sizes, uint32(len(s.Data)))
			f.durs = append(f.durs, s.Dur)
		}
		sampleNum += uint64(len(samples))
		frags = append(frags, f)
	}
	if sampleNum == 0 {
		return errors.New("the stream contained no audio samples")
	}
	if err := sw.Flush(); err != nil {
		return err
	}

	moov, err := buildMoov(init, trak, frags, tags)
	if err != nil {
		return err
	}

	// Chunk offsets are fixed-width, so the moov's size doesn't depend on
	// them: measure it with placeholders, then patch the real values in.
	ftyp := mp4.NewFtyp("M4A ", 0, []string{"M4A ", "mp42", "isom"})
	mdatPayloadStart := ftyp.Size() + moov.size() + 8
	if mdatPayloadStart+written > 1<<32-1 {
		return errors.New("file too large for 32-bit chunk offsets")
	}
	moov.setOffsets(mdatPayloadStart)

	if err := ftyp.Encode(out); err != nil {
		return err
	}
	if err := moov.box.Encode(out); err != nil {
		return err
	}
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(written+8))
	copy(hdr[4:], "mdat")
	if written+8 > 1<<32-1 {
		return errors.New("mdat too large")
	}
	if _, err := out.Write(hdr[:]); err != nil {
		return err
	}
	if _, err := spool.Seek(0, io.SeekStart); err != nil {
		return err
	}
	_, err = io.Copy(out, spool)
	return err
}

type builtMoov struct {
	box     *mp4.MoovBox
	stco    *mp4.StcoBox
	frags   []fragment
	offsets []uint64 // each chunk's start, relative to the mdat payload
}

func (m *builtMoov) size() uint64 { return m.box.Size() }

func (m *builtMoov) setOffsets(payloadStart uint64) {
	for i, rel := range m.offsets {
		m.stco.ChunkOffset[i] = uint32(payloadStart + rel)
	}
}

// buildMoov turns the init segment's moov into a progressive one: the fragment
// machinery goes, a sample table appears, and the tags ride along in udta.
func buildMoov(init *mp4.InitSegment, trak *mp4.TrakBox, frags []fragment, tags Tags) (*builtMoov, error) {
	moov := init.Moov
	stbl := trak.Mdia.Minf.Stbl
	stsd := stbl.Stsd
	if stsd == nil {
		return nil, errors.New("track has no sample description")
	}

	// Apple emits two identical sample entries; once encryption info is gone
	// nothing distinguishes them and some players reject the pair. A file with
	// anything else keeps every entry and the real indices.
	collapse := stsd.SampleCount == 2 && len(stsd.Children) == 2 &&
		stsd.Children[0].Type() == stsd.Children[1].Type()
	if collapse {
		stsd.Children = stsd.Children[:1]
		stsd.SampleCount = 1
	}

	var (
		stts  = &mp4.SttsBox{}
		stsc  = &mp4.StscBox{}
		stsz  = &mp4.StszBox{}
		stco  = &mp4.StcoBox{}
		total uint64
		rel   []uint64
	)
	var lastDelta uint32
	var lastDesc uint32
	for ci, f := range frags {
		for i, d := range f.durs {
			total += uint64(d)
			stsz.SampleSize = append(stsz.SampleSize, f.sizes[i])
			if n := len(stts.SampleCount); n > 0 && lastDelta == d {
				stts.SampleCount[n-1]++
			} else {
				stts.SampleCount = append(stts.SampleCount, 1)
				stts.SampleTimeDelta = append(stts.SampleTimeDelta, d)
				lastDelta = d
			}
		}
		desc := f.sampleDesc
		if collapse {
			desc = 1
		}
		// One chunk per fragment; only start a new stsc run when it changes.
		samples := uint32(len(f.sizes))
		if len(stsc.Entries) == 0 || stsc.Entries[len(stsc.Entries)-1].SamplesPerChunk != samples || lastDesc != desc {
			if err := stsc.AddEntry(uint32(ci+1), samples, desc); err != nil {
				return nil, err
			}
			lastDesc = desc
		}
		rel = append(rel, f.firstDelta)
		stco.ChunkOffset = append(stco.ChunkOffset, 0)
	}
	stsz.SampleNumber = uint32(len(stsz.SampleSize))

	newStbl := mp4.NewStblBox()
	newStbl.AddChild(stsd)
	newStbl.AddChild(stts)
	newStbl.AddChild(stsc)
	newStbl.AddChild(stsz)
	newStbl.AddChild(stco)
	minf := trak.Mdia.Minf
	for i, c := range minf.Children {
		if c.Type() == "stbl" {
			minf.Children[i] = newStbl
		}
	}
	minf.Stbl = newStbl

	// Durations: the track's own timescale, and the movie's for mvhd/tkhd/elst.
	mdhd := trak.Mdia.Mdhd
	if mdhd.Timescale == 0 {
		return nil, errors.New("track has a zero timescale")
	}
	mdhd.Duration = total
	movieTS := uint64(moov.Mvhd.Timescale)
	if movieTS == 0 {
		movieTS = uint64(mdhd.Timescale)
		moov.Mvhd.Timescale = mdhd.Timescale
	}
	movieDur := (total*movieTS + uint64(mdhd.Timescale)/2) / uint64(mdhd.Timescale)
	moov.Mvhd.Duration = movieDur
	trak.Tkhd.Duration = movieDur
	if trak.Edts != nil {
		// An edit list written for a fragmented file may carry duration 0
		// ("to the end"); a progressive reader needs the real figure.
		for _, el := range trak.Edts.Elst {
			for i := range el.Entries {
				if el.Entries[i].SegmentDuration == 0 {
					el.Entries[i].SegmentDuration = movieDur
				}
			}
		}
	}

	// No fragments left: drop mvex.
	moov.Mvex = nil
	kept := moov.Children[:0]
	for _, c := range moov.Children {
		switch c.Type() {
		case "mvex", "pssh":
			continue
		}
		kept = append(kept, c)
	}
	moov.Children = kept
	moov.Psshs, moov.Pssh = nil, nil

	if udta := tagBox(tags); udta != nil {
		moov.AddChild(udta)
	}
	return &builtMoov{box: moov, stco: stco, frags: frags, offsets: rel}, nil
}

// ---- tags -------------------------------------------------------------------

// rawBox is a box whose payload is already encoded; mp4ff has no types for the
// iTunes metadata atoms.
type rawBox struct {
	typ     string
	payload []byte
}

func (b *rawBox) Type() string { return b.typ }
func (b *rawBox) Size() uint64 { return 8 + uint64(len(b.payload)) }
func (b *rawBox) Encode(w io.Writer) error {
	var hdr [8]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(b.Size()))
	copy(hdr[4:], b.typ)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err := w.Write(b.payload)
	return err
}
func (b *rawBox) EncodeSW(sw bits.SliceWriter) error {
	sw.WriteUint32(uint32(b.Size()))
	sw.WriteString(b.typ, false)
	sw.WriteBytes(b.payload)
	return nil
}
func (b *rawBox) Info(w io.Writer, _, indent, _ string) error {
	_, err := fmt.Fprintf(w, "%s[%s] size=%d\n", indent, b.typ, b.Size())
	return err
}

func atom(typ string, payload []byte) []byte {
	out := make([]byte, 8+len(payload))
	binary.BigEndian.PutUint32(out, uint32(len(out)))
	copy(out[4:], typ)
	copy(out[8:], payload)
	return out
}

// dataAtom is an ilst item's `data` child. kind: 1 UTF-8, 0 implicit (binary),
// 13 JPEG, 14 PNG.
func dataAtom(kind uint32, value []byte) []byte {
	p := make([]byte, 8, 8+len(value))
	binary.BigEndian.PutUint32(p, kind) // version 0 + 24-bit type
	return atom("data", append(p, value...))
}

func textItem(key string, value string) []byte {
	if value == "" {
		return nil
	}
	return atom(key, dataAtom(1, []byte(value)))
}

// pairItem is trkn/disk: reserved, number, total (0 = unknown), reserved.
func pairItem(key string, n int) []byte {
	if n <= 0 {
		return nil
	}
	v := make([]byte, 8)
	binary.BigEndian.PutUint16(v[2:], uint16(n))
	return atom(key, dataAtom(0, v))
}

func tagBox(t Tags) *mp4.UdtaBox {
	var ilst []byte
	add := func(b []byte) { ilst = append(ilst, b...) }
	add(textItem("\xa9nam", t.Title))
	add(textItem("\xa9ART", t.Artist))
	add(textItem("\xa9alb", t.Album))
	add(textItem("aART", t.AlbumArtist))
	add(textItem("\xa9wrt", t.Composer))
	add(textItem("\xa9day", t.Date))
	add(textItem("\xa9gen", t.Genre))
	add(pairItem("trkn", t.Track))
	add(pairItem("disk", t.Disc))
	if len(t.Cover) > 0 {
		kind := uint32(13) // JPEG
		if len(t.Cover) >= 4 && string(t.Cover[1:4]) == "PNG" {
			kind = 14
		}
		add(atom("covr", dataAtom(kind, t.Cover)))
	}
	if len(ilst) == 0 {
		return nil
	}

	// hdlr: version/flags, pre_defined, 'mdir', reserved 'appl', 0, 0, name.
	hdlr := make([]byte, 0, 25)
	hdlr = append(hdlr, 0, 0, 0, 0, 0, 0, 0, 0)
	hdlr = append(hdlr, "mdir"...)
	hdlr = append(hdlr, "appl"...)
	hdlr = append(hdlr, make([]byte, 9)...)
	meta := append([]byte{0, 0, 0, 0}, atom("hdlr", hdlr)...) // meta is a full box
	meta = append(meta, atom("ilst", ilst)...)

	udta := &mp4.UdtaBox{}
	udta.AddChild(&rawBox{typ: "meta", payload: meta})
	return udta
}

// ---- reading ----------------------------------------------------------------

func readInit(r io.Reader) (*mp4.InitSegment, uint64, error) {
	var offset uint64
	init := mp4.NewMP4Init()
	for len(init.Children) < 2 {
		box, err := mp4.DecodeBox(offset, r)
		if err != nil {
			return nil, offset, err
		}
		offset += box.Size()
		switch box.Type() {
		case "ftyp", "moov":
			init.AddChild(box)
		default:
			// styp, free, … before the moov carry nothing we need.
		}
	}
	if init.Moov == nil {
		return nil, offset, errors.New("no moov box")
	}
	return init, offset, nil
}

// readFragment returns the next moof+mdat pair, or nil at a clean EOF.
func readFragment(r io.Reader, offset uint64) (*mp4.Fragment, uint64, error) {
	frag := mp4.NewFragment()
	for {
		box, err := mp4.DecodeBox(offset, r)
		if errors.Is(err, io.EOF) {
			return nil, offset, nil
		}
		if err != nil {
			return nil, offset, err
		}
		offset += box.Size()
		switch box.Type() {
		case "moof", "emsg", "prft":
			frag.AddChild(box)
		case "mdat":
			frag.AddChild(box)
			if frag.Moof == nil {
				return nil, offset, fmt.Errorf("mdat without a preceding moof")
			}
			return frag, offset, nil
		}
	}
}
