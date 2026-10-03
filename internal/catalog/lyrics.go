package catalog

import (
	"encoding/xml"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
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
		if out, ok := wordLevelLRC(raw); ok {
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
// `[00:08.789]<00:08.789>Hello <00:09.100>world<00:09.700>`: a line-level
// `[time]` tag, then one inline `<time>` tag per word, and a closing tag where
// the last word ends.
//
// Spacing is taken from the document, not normalised. Apple splits a sung
// word into syllable spans ("Hel" "lo") that have no whitespace between them,
// and the client needs to know which neighbours are glued to render "Hello"
// rather than "Hel lo" — a plain reader stripping `<...>` tags still gets the
// right text either way.
//
// Backing vocals (`ttm:role="x-bg"`) are timed on top of the main line, so
// folding them in would make the tags run backwards. They become a line of
// their own instead, and lines are emitted in time order.
func wordLevelLRC(raw string) (string, bool) {
	lines := parseSyllableLines(raw)
	if len(lines) == 0 {
		return "", false
	}
	sort.SliceStable(lines, func(i, j int) bool { return lines[i][0].start < lines[j][0].start })

	var b strings.Builder
	for _, words := range lines {
		fmt.Fprintf(&b, "[%s]", formatLRCTime(words[0].start))
		for i, w := range words {
			if i > 0 && w.spaceBefore {
				b.WriteByte(' ')
			}
			fmt.Fprintf(&b, "<%s>%s", formatLRCTime(w.start), w.text)
		}
		if last := words[len(words)-1]; last.end > last.start {
			fmt.Fprintf(&b, "<%s>", formatLRCTime(last.end))
		}
		b.WriteByte('\n')
	}
	return b.String(), true
}

type syllable struct {
	start, end  int
	text        string
	spaceBefore bool
}

// spanKind describes one open <span> while walking the document.
type spanKind struct {
	timed      bool
	start, end int
	bg         bool // backing vocal
	skip       bool // translation / transliteration, not the sung words
}

// lineBuf collects one line's words, remembering whether whitespace has been
// seen since the last one so the next word knows if it is glued on.
type lineBuf struct {
	words []syllable
	space bool
}

func (l *lineBuf) add(s syllable) {
	s.spaceBefore = s.spaceBefore || l.space
	l.space = false
	l.words = append(l.words, s)
}

// parseSyllableLines walks a word-timed TTML document and returns each sung
// line as its timed words. A <p> yields one line for its main vocal and, when
// it has backing vocals, a second for those.
func parseSyllableLines(raw string) [][]syllable {
	dec := xml.NewDecoder(strings.NewReader(raw))
	dec.Strict = false

	var (
		out      [][]syllable
		inP      bool
		main, bg lineBuf
		stack    []spanKind
	)
	flush := func() {
		if len(main.words) > 0 {
			out = append(out, main.words)
		}
		if len(bg.words) > 0 {
			out = append(out, bg.words)
		}
		main, bg, stack = lineBuf{}, lineBuf{}, nil
	}

	for {
		tok, err := dec.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			switch t.Name.Local {
			case "p":
				flush()
				inP = true
			case "span":
				if !inP {
					continue
				}
				k := spanKind{}
				var begin, end string
				for _, a := range t.Attr {
					switch a.Name.Local {
					case "begin":
						begin = a.Value
					case "end":
						end = a.Value
					case "role":
						switch a.Value {
						case "x-bg":
							k.bg = true
						case "x-translation", "x-roman":
							k.skip = true
						}
					}
				}
				if ms, ok := parseTTMLTime(begin); ok {
					k.timed, k.start, k.end = true, ms, ms
					if e, ok := parseTTMLTime(end); ok && e > ms {
						k.end = e
					}
				}
				stack = append(stack, k)
			}
		case xml.EndElement:
			switch t.Name.Local {
			case "p":
				flush()
				inP = false
			case "span":
				if len(stack) > 0 {
					stack = stack[:len(stack)-1]
				}
			}
		case xml.CharData:
			if !inP {
				continue
			}
			var isBG, skip bool
			var word *spanKind
			for i := range stack {
				isBG = isBG || stack[i].bg
				skip = skip || stack[i].skip
				if stack[i].timed {
					word = &stack[i]
				}
			}
			if skip {
				continue
			}
			buf := &main
			if isBG {
				buf = &bg
			}
			text := string(t)
			text2 := collapseWhitespace(text)
			if word == nil || text2 == "" {
				// Between words: whatever whitespace is here separates them.
				if text != "" {
					buf.space = true
				}
				continue
			}
			buf.add(syllable{
				start:       word.start,
				end:         word.end,
				text:        text2,
				spaceBefore: unicode.IsSpace(firstRune(text)),
			})
			// Trailing whitespace inside the span still separates it from
			// whatever follows.
			buf.space = unicode.IsSpace(lastRune(text))
		}
	}
	flush()
	return out
}

func firstRune(s string) rune {
	for _, r := range s {
		return r
	}
	return 0
}

func lastRune(s string) rune {
	r, _ := utf8.DecodeLastRuneInString(s)
	return r
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
