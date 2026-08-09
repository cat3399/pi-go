package session

import "unicode/utf8"

const utf8Replacement = "\xef\xbf\xbd"

// replaceInvalidUTF8LikeNode matches Node's UTF-8 StringDecoder replacement
// boundaries (the Unicode maximal-subpart rule). Go's encoding/json replaces
// invalid input one byte at a time, while bytes.ToValidUTF8 collapses adjacent
// invalid starters; neither is compatible with pi's readFile(..., "utf8")
// semantics. The original record is retained separately as Entry.RawJSON.
func replaceInvalidUTF8LikeNode(source []byte) ([]byte, bool) {
	if utf8.Valid(source) {
		return source, false
	}
	result := make([]byte, 0, len(source))
	copyStart := 0
	for index := 0; index < len(source); {
		lead := source[index]
		if lead < utf8.RuneSelf {
			index++
			continue
		}

		length := 0
		switch {
		case lead >= 0xc2 && lead <= 0xdf:
			length = 2
		case lead >= 0xe0 && lead <= 0xef:
			length = 3
		case lead >= 0xf0 && lead <= 0xf4:
			length = 4
		default:
			result = append(result, source[copyStart:index]...)
			result = append(result, utf8Replacement...)
			index++
			copyStart = index
			continue
		}

		consumed := length
		valid := true
		for offset := 1; offset < length; offset++ {
			if index+offset >= len(source) {
				consumed = offset
				valid = false
				break
			}
			continuation := source[index+offset]
			lower, upper := byte(0x80), byte(0xbf)
			if offset == 1 {
				switch lead {
				case 0xe0:
					lower = 0xa0
				case 0xed:
					upper = 0x9f
				case 0xf0:
					lower = 0x90
				case 0xf4:
					upper = 0x8f
				}
			}
			if continuation < lower || continuation > upper {
				// The offending byte is not consumed. Any valid prefix before it
				// forms the maximal ill-formed subpart represented by one U+FFFD.
				consumed = offset
				valid = false
				break
			}
		}
		if valid {
			index += length
			continue
		}
		result = append(result, source[copyStart:index]...)
		result = append(result, utf8Replacement...)
		index += consumed
		copyStart = index
	}
	result = append(result, source[copyStart:]...)
	return result, true
}
