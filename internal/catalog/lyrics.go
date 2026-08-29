package catalog

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Sync levels, in the order a caller should prefer them: word-by-word beats
// line-level beats no timing at all.
const (
	SyncWord = "word"
	SyncLine = "line"
	SyncNone = "none"
)

// Apple declares the sync tier directly on the TTML root via `itunes:timing`
// ("Word" | "Line" | "None") — confirmed against live responses, so this is
// read rather than guessed at by counting <span> elements. A `Line` document
// has zero <span>; a `None` document's <p> elements carry no begin/end
// attributes at all; a `Word` document nests one <span begin=.. end=..> per
// word inside each <p>.
//
// Go's encoding/xml matches attributes and elements by local name when the
// struct tag carries no namespace, so this decodes Apple's namespaced TTML
// (default namespace plus an "itunes:" prefix) without needing to declare
// either — verified against a real response and synthetic Word/None samples.
type ttmlDoc struct {
	Timing string   `xml:"timing,attr"`
	Body   ttmlBody `xml:"body"`
}

type ttmlBody struct {
	Divs []ttmlDiv `xml:"div"`
}

type ttmlDiv struct {
	Ps []ttmlP `xml:"p"`
}

type ttmlP struct {
	Begin    string     `xml:"begin,attr"`
	CharData string     `xml:",chardata"`
	Spans    []ttmlSpan `xml:"span"`
}

type ttmlSpan struct {
	Begin string `xml:"begin,attr"`
	Text  string `xml:",chardata"`
}

// ttmlToLRC converts an Apple Music lyrics TTML document into the best LRC
// tier it actually supports, cascading word → line → plain text if a tier's
// data turns out unusable (missing timestamps, no lines).
func ttmlToLRC(raw string) (lrc string, syncLevel string, err error) {
	var doc ttmlDoc
	dec := xml.NewDecoder(strings.NewReader(raw))
	dec.Strict = false
	if err := dec.Decode(&doc); err != nil {
		return "", "", fmt.Errorf("decode lyrics ttml: %w", err)
	}

	var lines []ttmlP
	for _, div := range doc.Body.Divs {
		lines = append(lines, div.Ps...)
	}
	if len(lines) == 0 {
		return "", "", errors.New("lyrics ttml has no lines")
	}

	switch strings.ToLower(doc.Timing) {
	case "word":
		if out, ok := wordLevelLRC(lines); ok {
			return out, SyncWord, nil
		}
		fallthrough
	case "line":
		if out, ok := lineLevelLRC(lines); ok {
			return out, SyncLine, nil
		}
		fallthrough
	default:
		return plainLRC(lines), SyncNone, nil
	}
}

// wordLevelLRC emits the informal "enhanced LRC" shape,
// `[00:08.789]<00:08.789>Hello <00:09.100>world`, one inline `<time>` tag per
// word ahead of a line-level `[time]` tag. Apple's own inter-word spacing is
// ignored in favour of a single space between words, both because it's
// inconsistent in practice and because it keeps the plain-text fallback (an
// LRC-unaware reader just stripping `<...>` tags) correctly spaced.
func wordLevelLRC(lines []ttmlP) (string, bool) {
	type word struct {
		ms   int
		text string
	}

	var b strings.Builder
	wrote := false
	for _, p := range lines {
		if len(p.Spans) == 0 {
			continue
		}
		var words []word
		for _, s := range p.Spans {
			text := strings.TrimSpace(s.Text)
			if text == "" {
				continue
			}
			ms, ok := parseTTMLTime(s.Begin)
			if !ok {
				continue
			}
			words = append(words, word{ms: ms, text: text})
		}
		if len(words) == 0 {
			continue
		}

		lineStart := words[0].ms
		if v, ok := parseTTMLTime(p.Begin); ok {
			lineStart = v
		}

		fmt.Fprintf(&b, "[%s]", formatLRCTime(lineStart))
		for i, w := range words {
			if i > 0 {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "<%s>%s", formatLRCTime(w.ms), w.text)
		}
		b.WriteByte('\n')
		wrote = true
	}
	return b.String(), wrote
}

// lineLevelLRC emits standard `[00:08.789]full line text` LRC.
func lineLevelLRC(lines []ttmlP) (string, bool) {
	var b strings.Builder
	wrote := false
	for _, p := range lines {
		text := collapseWhitespace(fullText(p))
		if text == "" {
			continue
		}
		ms, ok := parseTTMLTime(p.Begin)
		if !ok {
			continue
		}
		fmt.Fprintf(&b, "[%s]%s\n", formatLRCTime(ms), text)
		wrote = true
	}
	return b.String(), wrote
}

// plainLRC emits bare text lines with no time tags at all.
func plainLRC(lines []ttmlP) string {
	var b strings.Builder
	for _, p := range lines {
		text := collapseWhitespace(fullText(p))
		if text == "" {
			continue
		}
		b.WriteString(text)
		b.WriteByte('\n')
	}
	return b.String()
}

// fullText concatenates a <p>'s direct text with any span text — covers a
// document that wraps a line in one span without per-word timing, which some
// producers do even when the line otherwise has no word-level data.
func fullText(p ttmlP) string {
	if len(p.Spans) == 0 {
		return p.CharData
	}
	var b strings.Builder
	b.WriteString(p.CharData)
	for _, s := range p.Spans {
		b.WriteString(s.Text)
	}
	return b.String()
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// parseTTMLTime parses Apple's abbreviated clock-time format — "8.789"
// (seconds only) or "1:00.652" (minutes:seconds), and defensively
// "1:02:03.456" (hours:minutes:seconds) — into milliseconds.
func parseTTMLTime(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) > 3 {
		return 0, false
	}

	var h, m int
	var err error
	switch len(parts) {
	case 3:
		if h, err = strconv.Atoi(parts[0]); err != nil {
			return 0, false
		}
		if m, err = strconv.Atoi(parts[1]); err != nil {
			return 0, false
		}
	case 2:
		if m, err = strconv.Atoi(parts[0]); err != nil {
			return 0, false
		}
	}

	sec, err := strconv.ParseFloat(parts[len(parts)-1], 64)
	if err != nil {
		return 0, false
	}
	total := (float64(h*3600+m*60) + sec) * 1000
	return int(math.Round(total)), true
}

// formatLRCTime renders milliseconds as LRC's `mm:ss.mmm`.
func formatLRCTime(ms int) string {
	if ms < 0 {
		ms = 0
	}
	totalSec := ms / 1000
	m := totalSec / 60
	s := totalSec % 60
	frac := ms % 1000
	return fmt.Sprintf("%02d:%02d.%03d", m, s, frac)
}
