package main

const observedPathHashSizeMaskAll uint8 = 0b111

// observedPathHashSizeMask returns one evidence bit for a persisted
// observation path. A path is evidence only when it is non-empty and every
// hop is hexadecimal, has the same width, and is exactly 1, 2, or 3 bytes.
// Empty/direct paths and malformed or mixed-width paths are deliberately
// unknown: they must not be interpreted as a sender setting.
func observedPathHashSizeMask(pathJSON string) uint8 {
	i := 0
	skipJSONSpace(pathJSON, &i)
	if i >= len(pathJSON) || pathJSON[i] != '[' {
		return 0
	}
	i++
	skipJSONSpace(pathJSON, &i)
	if i >= len(pathJSON) || pathJSON[i] == ']' {
		return 0
	}

	width := 0
	for {
		skipJSONSpace(pathJSON, &i)
		if i >= len(pathJSON) || pathJSON[i] != '"' {
			return 0
		}
		i++
		start := i
		for i < len(pathJSON) && pathJSON[i] != '"' {
			if !isHexByte(pathJSON[i]) {
				return 0
			}
			i++
		}
		if i >= len(pathJSON) || i == start {
			return 0
		}
		hopWidth := i - start
		if hopWidth != 2 && hopWidth != 4 && hopWidth != 6 {
			return 0
		}
		if width == 0 {
			width = hopWidth
		} else if hopWidth != width {
			return 0
		}
		i++
		skipJSONSpace(pathJSON, &i)
		if i >= len(pathJSON) {
			return 0
		}
		switch pathJSON[i] {
		case ',':
			i++
		case ']':
			i++
			skipJSONSpace(pathJSON, &i)
			if i != len(pathJSON) {
				return 0
			}
			return 1 << (uint(width/2) - 1)
		default:
			return 0
		}
	}
}

func skipJSONSpace(s string, i *int) {
	for *i < len(s) {
		switch s[*i] {
		case ' ', '\t', '\n', '\r':
			*i = *i + 1
		default:
			return
		}
	}
}

func isHexByte(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// observedPathHashSizes expands a compact evidence mask into the stable API
// order. It always returns a non-nil slice so unknown evidence serializes as
// [] rather than null.
func observedPathHashSizes(mask uint8) []int {
	mask &= observedPathHashSizeMaskAll
	sizes := make([]int, 0, 3)
	for size := 1; size <= 3; size++ {
		if mask&(1<<uint(size-1)) != 0 {
			sizes = append(sizes, size)
		}
	}
	return sizes
}

func (tx *StoreTx) mergeObservedPathHashSize(pathJSON string) {
	if tx == nil || tx.PayloadType == nil || *tx.PayloadType != PayloadGRP_TXT {
		return
	}
	tx.pathHashSizeMask |= observedPathHashSizeMask(pathJSON)
}

func (tx *StoreTx) observedPathHashSizes() []int {
	if tx == nil {
		return []int{}
	}
	return observedPathHashSizes(tx.pathHashSizeMask)
}
