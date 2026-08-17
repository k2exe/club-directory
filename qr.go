package main

// A minimal QR encoder: byte mode, error-correction level L, versions 1-9.
// That covers otpauth:// URIs comfortably (232 bytes at version 9) and keeps
// the binary dependency-free, which matters for an air-gapped deployment.

import (
	"errors"
	"fmt"
	"strings"
)

// dataCodewords[v] / ecPerBlock[v] / blocks[v] for level L, indexed by version.
var qrDataCodewords = [10]int{0, 19, 34, 55, 80, 108, 136, 156, 194, 232}
var qrECPerBlock = [10]int{0, 7, 10, 15, 20, 26, 18, 20, 24, 30}
var qrBlocks = [10]int{0, 1, 1, 1, 1, 1, 2, 2, 2, 2}
var qrRemainderBits = [10]int{0, 0, 7, 7, 7, 7, 7, 0, 0, 0}

var qrAlignCenters = [10][]int{
	{}, {}, {6, 18}, {6, 22}, {6, 26}, {6, 30}, {6, 34}, {6, 22, 38}, {6, 24, 42}, {6, 26, 46},
}

// ---------- GF(256) ----------

var gfExp [512]byte
var gfLog [256]byte

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11d
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

func rsGenerator(n int) []byte {
	g := []byte{1}
	for i := 0; i < n; i++ {
		next := make([]byte, len(g)+1)
		for j, c := range g {
			next[j] ^= c
			next[j+1] ^= gfMul(c, gfExp[i])
		}
		g = next
	}
	return g
}

func rsEncode(data []byte, n int) []byte {
	gen := rsGenerator(n)
	rem := make([]byte, n)
	for _, d := range data {
		factor := d ^ rem[0]
		copy(rem, rem[1:])
		rem[n-1] = 0
		for i, g := range gen[1:] {
			rem[i] ^= gfMul(g, factor)
		}
	}
	return rem
}

// ---------- bit buffer ----------

type bitBuf struct {
	bits []bool
}

func (b *bitBuf) add(val, n int) {
	for i := n - 1; i >= 0; i-- {
		b.bits = append(b.bits, val&(1<<uint(i)) != 0)
	}
}

func (b *bitBuf) bytes() []byte {
	out := make([]byte, (len(b.bits)+7)/8)
	for i, bit := range b.bits {
		if bit {
			out[i/8] |= 1 << uint(7-i%8)
		}
	}
	return out
}

// ---------- encoding ----------

type qrMatrix struct {
	size int
	mod  []bool // dark
	fn   []bool // function module, not maskable
}

func newMatrix(size int) *qrMatrix {
	return &qrMatrix{size: size, mod: make([]bool, size*size), fn: make([]bool, size*size)}
}

func (m *qrMatrix) at(r, c int) bool     { return m.mod[r*m.size+c] }
func (m *qrMatrix) isFn(r, c int) bool   { return m.fn[r*m.size+c] }
func (m *qrMatrix) set(r, c int, v bool) { m.mod[r*m.size+c] = v }
func (m *qrMatrix) setFn(r, c int, v bool) {
	m.mod[r*m.size+c] = v
	m.fn[r*m.size+c] = true
}
func (m *qrMatrix) in(r, c int) bool { return r >= 0 && c >= 0 && r < m.size && c < m.size }

// QREncode builds the module matrix for text.
func QREncode(text string) (*qrMatrix, error) {
	raw := []byte(text)
	version := 0
	for v := 1; v <= 9; v++ {
		// 4 mode bits + 8 count bits + payload must fit the data capacity.
		if len(raw)+2 <= qrDataCodewords[v] {
			version = v
			break
		}
	}
	if version == 0 {
		return nil, errors.New("qr: payload too long for version 9")
	}

	capacity := qrDataCodewords[version]
	var bb bitBuf
	bb.add(0b0100, 4)   // byte mode
	bb.add(len(raw), 8) // character count (8 bits for versions 1-9)
	for _, x := range raw {
		bb.add(int(x), 8)
	}
	// Terminator, then pad to a byte boundary.
	for i := 0; i < 4 && len(bb.bits) < capacity*8; i++ {
		bb.bits = append(bb.bits, false)
	}
	for len(bb.bits)%8 != 0 {
		bb.bits = append(bb.bits, false)
	}
	dataBytes := bb.bytes()
	for i := 0; len(dataBytes) < capacity; i++ {
		if i%2 == 0 {
			dataBytes = append(dataBytes, 0xEC)
		} else {
			dataBytes = append(dataBytes, 0x11)
		}
	}

	// Split into equal blocks (true for every version 1-9 at level L).
	nb := qrBlocks[version]
	ecLen := qrECPerBlock[version]
	perBlock := capacity / nb
	dataBlocks := make([][]byte, nb)
	ecBlocks := make([][]byte, nb)
	for i := 0; i < nb; i++ {
		dataBlocks[i] = dataBytes[i*perBlock : (i+1)*perBlock]
		ecBlocks[i] = rsEncode(dataBlocks[i], ecLen)
	}
	// Interleave.
	final := make([]byte, 0, capacity+nb*ecLen)
	for i := 0; i < perBlock; i++ {
		for b := 0; b < nb; b++ {
			final = append(final, dataBlocks[b][i])
		}
	}
	for i := 0; i < ecLen; i++ {
		for b := 0; b < nb; b++ {
			final = append(final, ecBlocks[b][i])
		}
	}

	size := 17 + 4*version
	m := newMatrix(size)
	m.drawFunctionPatterns(version)

	// Zig-zag placement of the interleaved codeword stream.
	bits := make([]bool, 0, len(final)*8+qrRemainderBits[version])
	for _, b := range final {
		for i := 7; i >= 0; i-- {
			bits = append(bits, b&(1<<uint(i)) != 0)
		}
	}
	for i := 0; i < qrRemainderBits[version]; i++ {
		bits = append(bits, false)
	}
	idx := 0
	upward := true
	for right := size - 1; right >= 0; right -= 2 {
		if right == 6 {
			right = 5 // the vertical timing pattern occupies column 6
		}
		for i := 0; i < size; i++ {
			row := i
			if upward {
				row = size - 1 - i
			}
			for c := 0; c < 2; c++ {
				col := right - c
				if col < 0 || m.isFn(row, col) {
					continue
				}
				if idx < len(bits) {
					m.set(row, col, bits[idx])
					idx++
				}
			}
		}
		upward = !upward
	}

	// Choose the mask with the lowest penalty.
	best, bestScore := 0, 1<<62
	for mask := 0; mask < 8; mask++ {
		trial := m.withMask(mask, version)
		if s := trial.penalty(); s < bestScore {
			best, bestScore = mask, s
		}
	}
	return m.withMask(best, version), nil
}

func (m *qrMatrix) copy() *qrMatrix {
	c := newMatrix(m.size)
	copy(c.mod, m.mod)
	copy(c.fn, m.fn)
	return c
}

func maskFn(mask, r, c int) bool {
	switch mask {
	case 0:
		return (r+c)%2 == 0
	case 1:
		return r%2 == 0
	case 2:
		return c%3 == 0
	case 3:
		return (r+c)%3 == 0
	case 4:
		return (r/2+c/3)%2 == 0
	case 5:
		return (r*c)%2+(r*c)%3 == 0
	case 6:
		return ((r*c)%2+(r*c)%3)%2 == 0
	case 7:
		return ((r+c)%2+(r*c)%3)%2 == 0
	}
	return false
}

func (m *qrMatrix) withMask(mask, version int) *qrMatrix {
	out := m.copy()
	for r := 0; r < out.size; r++ {
		for c := 0; c < out.size; c++ {
			if !out.isFn(r, c) && maskFn(mask, r, c) {
				out.set(r, c, !out.at(r, c))
			}
		}
	}
	out.drawFormat(mask)
	if version >= 7 {
		out.drawVersion(version)
	}
	return out
}

func (m *qrMatrix) drawFunctionPatterns(version int) {
	size := m.size
	// Finder patterns plus separators.
	for _, p := range [][2]int{{0, 0}, {0, size - 7}, {size - 7, 0}} {
		for dr := -1; dr <= 7; dr++ {
			for dc := -1; dc <= 7; dc++ {
				r, c := p[0]+dr, p[1]+dc
				if !m.in(r, c) {
					continue
				}
				dark := (dr >= 0 && dr <= 6 && (dc == 0 || dc == 6)) ||
					(dc >= 0 && dc <= 6 && (dr == 0 || dr == 6)) ||
					(dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4)
				m.setFn(r, c, dark)
			}
		}
	}
	// Timing patterns.
	for i := 8; i < size-8; i++ {
		dark := i%2 == 0
		m.setFn(6, i, dark)
		m.setFn(i, 6, dark)
	}
	// Alignment patterns, skipping any that would collide with a finder.
	centers := qrAlignCenters[version]
	for _, r := range centers {
		for _, c := range centers {
			if (r <= 8 && c <= 8) || (r <= 8 && c >= size-9) || (r >= size-9 && c <= 8) {
				continue
			}
			for dr := -2; dr <= 2; dr++ {
				for dc := -2; dc <= 2; dc++ {
					dark := dr == -2 || dr == 2 || dc == -2 || dc == 2 || (dr == 0 && dc == 0)
					m.setFn(r+dr, c+dc, dark)
				}
			}
		}
	}
	// Reserve the format information areas.
	for i := 0; i <= 8; i++ {
		if i != 6 {
			m.setFn(8, i, false)
			m.setFn(i, 8, false)
		}
	}
	for i := 0; i < 8; i++ {
		m.setFn(size-1-i, 8, false)
		m.setFn(8, size-1-i, false)
	}
	// Always-dark module.
	m.setFn(size-8, 8, true)
	// Reserve version information blocks.
	if version >= 7 {
		for i := 0; i < 18; i++ {
			r, c := i/3, size-11+i%3
			m.setFn(r, c, false)
			m.setFn(c, r, false)
		}
	}
}

func bchFormat(d int) int {
	v := d << 10
	for i := 14; i >= 10; i-- {
		if v&(1<<uint(i)) != 0 {
			v ^= 0x537 << uint(i-10)
		}
	}
	return ((d << 10) | v) ^ 0x5412
}

func bchVersion(ver int) int {
	v := ver << 12
	for i := 17; i >= 12; i-- {
		if v&(1<<uint(i)) != 0 {
			v ^= 0x1f25 << uint(i-12)
		}
	}
	return (ver << 12) | v
}

func (m *qrMatrix) drawFormat(mask int) {
	size := m.size
	bits := bchFormat(0b01<<3 | mask) // 0b01 = error correction level L
	get := func(i int) bool { return bits&(1<<uint(i)) != 0 }
	for i := 0; i <= 5; i++ {
		m.setFn(i, 8, get(i))
	}
	m.setFn(7, 8, get(6))
	m.setFn(8, 8, get(7))
	m.setFn(8, 7, get(8))
	for i := 9; i <= 14; i++ {
		m.setFn(8, 14-i, get(i))
	}
	for i := 0; i <= 7; i++ {
		m.setFn(8, size-1-i, get(i))
	}
	for i := 8; i <= 14; i++ {
		m.setFn(size-15+i, 8, get(i))
	}
	m.setFn(size-8, 8, true)
}

func (m *qrMatrix) drawVersion(version int) {
	bits := bchVersion(version)
	size := m.size
	for i := 0; i < 18; i++ {
		v := bits&(1<<uint(i)) != 0
		r, c := i/3, size-11+i%3
		m.setFn(r, c, v)
		m.setFn(c, r, v)
	}
}

func (m *qrMatrix) penalty() int {
	size := m.size
	score := 0
	// Rule 1: runs of five or more identical modules.
	runScore := func(get func(i, j int) bool) int {
		s := 0
		for i := 0; i < size; i++ {
			run, prev := 1, get(i, 0)
			for j := 1; j < size; j++ {
				cur := get(i, j)
				if cur == prev {
					run++
				} else {
					if run >= 5 {
						s += 3 + run - 5
					}
					run, prev = 1, cur
				}
			}
			if run >= 5 {
				s += 3 + run - 5
			}
		}
		return s
	}
	score += runScore(func(i, j int) bool { return m.at(i, j) })
	score += runScore(func(i, j int) bool { return m.at(j, i) })

	// Rule 2: 2x2 blocks of one colour.
	for r := 0; r < size-1; r++ {
		for c := 0; c < size-1; c++ {
			v := m.at(r, c)
			if v == m.at(r, c+1) && v == m.at(r+1, c) && v == m.at(r+1, c+1) {
				score += 3
			}
		}
	}

	// Rule 3: finder-like sequences.
	pat1 := []bool{true, false, true, true, true, false, true, false, false, false, false}
	pat2 := []bool{false, false, false, false, true, false, true, true, true, false, true}
	match := func(get func(i int) bool, start int, pat []bool) bool {
		for k, want := range pat {
			if get(start+k) != want {
				return false
			}
		}
		return true
	}
	for i := 0; i < size; i++ {
		row := func(j int) bool { return m.at(i, j) }
		col := func(j int) bool { return m.at(j, i) }
		for j := 0; j+11 <= size; j++ {
			if match(row, j, pat1) || match(row, j, pat2) {
				score += 40
			}
			if match(col, j, pat1) || match(col, j, pat2) {
				score += 40
			}
		}
	}

	// Rule 4: deviation from an even balance of dark and light.
	dark := 0
	for _, v := range m.mod {
		if v {
			dark++
		}
	}
	pct := dark * 100 / (size * size)
	dev := pct - 50
	if dev < 0 {
		dev = -dev
	}
	score += (dev / 5) * 10
	return score
}

// SVG renders the code as a scalable image, sized in CSS by the caller.
func (m *qrMatrix) SVG() string {
	const quiet = 4
	dim := m.size + quiet*2
	var path strings.Builder
	for r := 0; r < m.size; r++ {
		for c := 0; c < m.size; c++ {
			if m.at(r, c) {
				fmt.Fprintf(&path, "M%d %dh1v1h-1z", c+quiet, r+quiet)
			}
		}
	}
	return fmt.Sprintf(
		`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges" role="img" aria-label="Enrolment QR code">`+
			`<rect width="%d" height="%d" fill="#fff"/><path d="%s" fill="#12242f"/></svg>`,
		dim, dim, dim, dim, path.String())
}

func qrSVG(text string) (string, error) {
	m, err := QREncode(text)
	if err != nil {
		return "", err
	}
	return m.SVG(), nil
}
