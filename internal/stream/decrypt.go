// Package stream turns an Apple Music track into a decrypted, playable
// fragmented MP4.
//
// The cbcs sample-decryption logic in this file is adapted from
// github.com/zhaarey/apple-music-downloader (utils/runv2), which is the
// reference implementation for talking to the wrapper daemon's decryption
// service. It is vendored rather than imported because that project's module is
// literally named "main" and cannot be imported.
package stream

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/grafov/m3u8"
	"github.com/itouakirai/mp4ff/mp4"
)

// prefetchKey is the placeholder key URI Apple uses for a track's first
// segment. The daemon expects adam id "0" for it rather than the real one.
const prefetchKey = "skd://itunes.apple.com/P000000000/s1/e1"

// closeDecryptor resets the daemon's loops and closes the connection.
func closeDecryptor(conn io.WriteCloser) error {
	defer conn.Close()
	_, err := conn.Write([]byte{0, 0, 0, 0, 0})
	return err
}

// switchKeys breaks the daemon's inner sample loop so a new key can be sent.
func switchKeys(conn io.Writer) error {
	_, err := conn.Write([]byte{0, 0, 0, 0})
	return err
}

// sendString writes a length-prefixed string: an adam id or a key URI.
func sendString(conn io.Writer, s string) error {
	if len(s) > 255 {
		return fmt.Errorf("string too long for the daemon protocol: %d bytes", len(s))
	}
	if _, err := conn.Write([]byte{byte(len(s))}); err != nil {
		return err
	}
	_, err := io.WriteString(conn, s)
	return err
}

// cbcsFullSubsampleDecrypt sends a whole protected range in one go. Apple Music
// ALAC uses full-subsample encryption, so this is the hot path.
func cbcsFullSubsampleDecrypt(data []byte, conn *bufio.ReadWriter) error {
	// The daemon only transforms whole 16-byte blocks; the tail is left as-is.
	truncatedLen := len(data) & ^0xf
	if truncatedLen == 0 {
		return nil
	}
	if err := binary.Write(conn, binary.LittleEndian, uint32(truncatedLen)); err != nil {
		return err
	}
	if _, err := conn.Write(data[:truncatedLen]); err != nil {
		return err
	}
	if err := conn.Flush(); err != nil {
		return err
	}
	_, err := io.ReadFull(conn, data[:truncatedLen])
	return err
}

// cbcsStripeDecrypt handles pattern encryption, where only every Nth block is
// encrypted. Used by video codecs rather than Apple Music audio.
func cbcsStripeDecrypt(data []byte, conn *bufio.ReadWriter, decryptBlockLen, skipBlockLen int) error {
	size := len(data)
	if size < decryptBlockLen {
		return nil
	}

	count := ((size - decryptBlockLen) / (decryptBlockLen + skipBlockLen)) + 1
	if err := binary.Write(conn, binary.LittleEndian, uint32(count*decryptBlockLen)); err != nil {
		return err
	}

	for pos := 0; ; {
		if size-pos < decryptBlockLen {
			break
		}
		if _, err := conn.Write(data[pos : pos+decryptBlockLen]); err != nil {
			return err
		}
		pos += decryptBlockLen
		if size-pos < skipBlockLen {
			break
		}
		pos += skipBlockLen
	}
	if err := conn.Flush(); err != nil {
		return err
	}

	for pos := 0; ; {
		if size-pos < decryptBlockLen {
			break
		}
		if _, err := io.ReadFull(conn, data[pos:pos+decryptBlockLen]); err != nil {
			return err
		}
		pos += decryptBlockLen
		if size-pos < skipBlockLen {
			break
		}
		pos += skipBlockLen
	}
	return nil
}

func cbcsDecryptRaw(data []byte, conn *bufio.ReadWriter, decryptBlockLen, skipBlockLen int) error {
	if skipBlockLen == 0 {
		return cbcsFullSubsampleDecrypt(data, conn)
	}
	return cbcsStripeDecrypt(data, conn, decryptBlockLen, skipBlockLen)
}

// cbcsDecryptSample decrypts one sample in place.
func cbcsDecryptSample(sample []byte, conn *bufio.ReadWriter,
	subSamples []mp4.SubSamplePattern, tenc *mp4.TencBox) error {

	decryptBlockLen := int(tenc.DefaultCryptByteBlock) * 16
	skipBlockLen := int(tenc.DefaultSkipByteBlock) * 16

	if len(subSamples) == 0 {
		return cbcsDecryptRaw(sample, conn, decryptBlockLen, skipBlockLen)
	}

	var pos uint32
	for _, ss := range subSamples {
		pos += uint32(ss.BytesOfClearData)
		if ss.BytesOfProtectedData <= 0 {
			continue
		}
		if err := cbcsDecryptRaw(sample[pos:pos+ss.BytesOfProtectedData],
			conn, decryptBlockLen, skipBlockLen); err != nil {
			return err
		}
		pos += ss.BytesOfProtectedData
	}
	return nil
}

// decryptFragment decrypts every sample in a fragment and strips the boxes that
// describe the encryption, leaving a plain playable fragment.
func decryptFragment(frag *mp4.Fragment, tracks map[uint32]mp4.DecryptTrackInfo, conn *bufio.ReadWriter) error {
	moof := frag.Moof
	var bytesRemoved uint64

	for _, traf := range moof.Trafs {
		ti, ok := tracks[traf.Tfhd.TrackID]
		if !ok {
			return fmt.Errorf("no decryption info for track %d", traf.Tfhd.TrackID)
		}
		if ti.Sinf == nil {
			continue // unencrypted track
		}
		if st := ti.Sinf.Schm.SchemeType; st != "cbcs" {
			return fmt.Errorf("unsupported encryption scheme %q", st)
		}

		hasSenc, isParsed := traf.ContainsSencBox()
		if !hasSenc {
			return errors.New("fragment has no senc box")
		}
		senc := traf.Senc
		if senc == nil {
			senc = traf.UUIDSenc.Senc
		}
		if !isParsed {
			if err := senc.ParseReadBox(ti.Sinf.Schi.Tenc.DefaultPerSampleIVSize, traf.Saiz); err != nil {
				return err
			}
		}

		samples, err := frag.GetFullSamples(ti.Trex)
		if err != nil {
			return err
		}
		for i := range samples {
			var subSamples []mp4.SubSamplePattern
			if len(senc.SubSamples) != 0 {
				subSamples = senc.SubSamples[i]
			}
			if err := cbcsDecryptSample(samples[i].Data, conn, subSamples, ti.Sinf.Schi.Tenc); err != nil {
				return err
			}
		}

		bytesRemoved += traf.RemoveEncryptionBoxes()
	}

	_, psshRemoved := moof.RemovePsshs()
	bytesRemoved += psshRemoved
	for _, traf := range moof.Trafs {
		for _, trun := range traf.Truns {
			trun.DataOffset -= int32(bytesRemoved)
		}
	}
	return nil
}

// transformInit collects per-track decryption info and drops encryption-related
// sample grouping boxes from the init segment.
func transformInit(init *mp4.InitSegment) (map[uint32]mp4.DecryptTrackInfo, error) {
	di, err := mp4.DecryptInit(init)
	tracks := make(map[uint32]mp4.DecryptTrackInfo, len(di.TrackInfos))
	for _, ti := range di.TrackInfos {
		tracks[ti.TrackID] = ti
	}
	if err != nil {
		return tracks, err
	}
	for _, trak := range init.Moov.Traks {
		stbl := trak.Mdia.Minf.Stbl
		stbl.Children, _ = filterSbgpSgpd(stbl.Children)
	}
	return tracks, nil
}

func filterSbgpSgpd(children []mp4.Box) ([]mp4.Box, uint64) {
	var bytesRemoved uint64
	out := make([]mp4.Box, 0, len(children))
	for _, child := range children {
		switch box := child.(type) {
		case *mp4.SbgpBox:
			if box.GroupingType == "seam" || box.GroupingType == "seig" {
				bytesRemoved += child.Size()
				continue
			}
		case *mp4.SgpdBox:
			if box.GroupingType == "seam" || box.GroupingType == "seig" {
				bytesRemoved += child.Size()
				continue
			}
		}
		out = append(out, child)
	}
	return out, bytesRemoved
}

// sanitizeInit drops the duplicate sample entry Apple emits. Both entries become
// identical once encryption info is removed, and some players reject two.
func sanitizeInit(init *mp4.InitSegment) error {
	traks := init.Moov.Traks
	if len(traks) != 1 {
		return fmt.Errorf("expected exactly 1 track, got %d", len(traks))
	}
	stsd := traks[0].Mdia.Minf.Stbl.Stsd
	if stsd.SampleCount == 1 {
		return nil
	}
	if stsd.SampleCount > 2 {
		return fmt.Errorf("expected 1 or 2 stsd entries, got %d", stsd.SampleCount)
	}
	if stsd.Children[0].Type() != stsd.Children[1].Type() {
		return errors.New("stsd children are of different types")
	}
	stsd.Children = stsd.Children[:1]
	stsd.SampleCount = 1
	return nil
}

func readInitSegment(r io.Reader) (*mp4.InitSegment, uint64, error) {
	var offset uint64
	init := mp4.NewMP4Init()
	for i := 0; i < 2; i++ {
		box, err := mp4.DecodeBox(offset, r)
		if err != nil {
			return nil, offset, err
		}
		if t := box.Type(); t != "ftyp" && t != "moov" {
			return nil, offset, fmt.Errorf("unexpected box %q, want ftyp or moov", t)
		}
		init.AddChild(box)
		offset += box.Size()
	}
	return init, offset, nil
}

// readNextFragment returns the next moof+mdat pair, or nil at EOF.
func readNextFragment(r io.Reader, offset uint64) (*mp4.Fragment, uint64, error) {
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
				return nil, offset, fmt.Errorf("mdat without a preceding moof at offset %d", offset)
			}
			return frag, offset, nil
		}
		// Anything else mid-stream is ignored.
	}
}

// filterPlaylist strips PlayReady and Widevine key lines, which the m3u8
// library cannot represent alongside Apple's own.
func filterPlaylist(r io.Reader) (*bytes.Buffer, error) {
	buf := &bytes.Buffer{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)

	prefix := []byte("#EXT-X-KEY:")
	keep := []byte("streamingkeydelivery")
	for sc.Scan() {
		line := sc.Bytes()
		if bytes.HasPrefix(line, prefix) && !bytes.Contains(line, keep) {
			continue
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	return buf, sc.Err()
}

func parseMediaPlaylist(r io.Reader) ([]*m3u8.MediaSegment, error) {
	buf, err := filterPlaylist(r)
	if err != nil {
		return nil, err
	}
	playlist, listType, err := m3u8.Decode(*buf, true)
	if err != nil {
		return nil, err
	}
	if listType != m3u8.MEDIA {
		return nil, errors.New("expected a media playlist")
	}
	return playlist.(*m3u8.MediaPlaylist).Segments, nil
}
