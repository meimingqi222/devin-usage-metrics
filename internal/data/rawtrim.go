package data

import "bytes"

// rawKeep bounds JSON string literals before decoding. Transcripts embed
// whole tool outputs and images in single strings; decoding them only for
// slim() to cut them made Claude loads spend most of their time in GC.
const rawKeep = 64 << 10

// trimLongStrings shortens string literals longer than rawKeep raw bytes so
// that slim() still yields the same value: a kept literal decodes to at
// least rawKeep/6 bytes (every \uXXXX escape is six raw bytes for at least
// one decoded byte) and contains over 100 visible runes, counted only from
// unescaped ASCII, so titlePrefix's cut lies inside it. A literal is cut
// only between whole characters and escapes. Lines too short to hold such a
// literal are returned unchanged and uncopied.
func trimLongStrings(line []byte) []byte {
	if len(line) <= rawKeep {
		return line
	}
	var out []byte
	copied := 0 // line[:copied] is already in out
	for i := 0; i < len(line); {
		q := bytes.IndexByte(line[i:], '"')
		if q < 0 {
			break
		}
		i += q + 1
		start, visible, cut := i, 0, -1
		for i < len(line) && line[i] != '"' {
			if line[i] == '\\' {
				if i+1 < len(line) && line[i+1] == 'u' {
					i += 6
				} else {
					i += 2
				}
			} else {
				if c := line[i]; c > ' ' && c < 0x80 {
					visible++
				}
				i++
			}
			if i-start >= rawKeep && visible > 100 && i < len(line) && line[i]&0xC0 != 0x80 {
				cut = i
				// Validate discarded bytes too, so trimming cannot hide an
				// invalid escape or unescaped control character.
				var valid bool
				i, valid = literalEnd(line, i)
				if !valid {
					return line
				}
				break
			}
		}
		if i >= len(line) {
			break
		}
		// Object keys must remain exact; slim only shortens values.
		after := bytes.TrimSpace(line[i+1:])
		if cut >= 0 && (len(after) == 0 || after[0] != ':') {
			if out == nil {
				out = make([]byte, 0, len(line)/4)
			}
			// cut follows a whole escape, so a trailing `\\` stays an
			// escaped backslash and cannot escape the added quote.
			out = append(out, line[copied:cut]...)
			out = append(out, '"')
			copied = i + 1
		}
		i++ // past the closing quote
	}
	if out == nil {
		return line
	}
	return append(out, line[copied:]...)
}

// literalEnd finds the closing quote after a complete escape boundary.
func literalEnd(line []byte, i int) (int, bool) {
	for i < len(line) {
		c := line[i]
		switch {
		case c == '"':
			return i, true
		case c < 0x20:
			return 0, false
		case c == '\\':
			i++
			if i >= len(line) {
				return 0, false
			}
			switch line[i] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
			case 'u':
				for range 4 {
					i++
					if i >= len(line) {
						return 0, false
					}
					c := line[i]
					if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
						return 0, false
					}
				}
			default:
				return 0, false
			}
		}
		i++
	}
	return 0, false
}
