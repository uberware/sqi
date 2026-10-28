// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"bufio"
	"slices"
	"unicode/utf8"
)

// maxOutputLine is the most bytes of one task output line delivered as a
// single piece. A longer line is delivered as consecutive pieces of at most
// this many bytes. It is bufio.Scanner's default token limit, so lines that
// were delivered whole before still are.
const maxOutputLine = bufio.MaxScanTokenSize

// lineSplitter turns the slices bufio.Reader.ReadSlice returns into lines and,
// for a line longer than maxOutputLine, pieces. emit must not retain the slice
// it is given: it may be the reader's own buffer.
type lineSplitter struct {
	emit func([]byte)
	// onSplit is called for every piece of a split line and must not be nil;
	// the caller deduplicates (scanOutput passes a sync.OnceFunc).
	onSplit func()
	// carry holds the tail of the last buffer that was not yet delivered: at
	// first an incomplete trailing UTF-8 sequence or a '\r' that may precede
	// '\n', growing by the bytes each later cut backs off over until the line
	// ends. It is always its own copy, never a view of the reader's buffer.
	carry []byte
	// split is true once the current line has delivered a piece.
	split bool
}

// partial takes a buffer-full of a line whose end has not been read yet.
func (s *lineSplitter) partial(chunk []byte) {
	b := s.join(chunk)
	n := pieceLen(b)
	s.deliverPiece(b[:n])
	s.carry = slices.Clone(b[n:])
}

// end takes the last bytes of a line, without its '\n'. It is also called at
// EOF with whatever remains, which ends the final unterminated line.
func (s *lineSplitter) end(chunk []byte) {
	b := s.join(chunk)
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	for len(b) > maxOutputLine {
		n := pieceLen(b)
		s.deliverPiece(b[:n])
		b = b[n:]
	}
	// A line that ended exactly on a piece boundary leaves nothing to deliver;
	// an empty line that was never split is still a line.
	if !s.split || len(b) > 0 {
		s.emit(b)
	}
	s.split = false
}

func (s *lineSplitter) deliverPiece(p []byte) {
	s.split = true
	s.onSplit()
	s.emit(p)
}

// join returns carry followed by chunk. With nothing carried that is chunk
// itself, so the common short line is copied only once, by emit.
func (s *lineSplitter) join(chunk []byte) []byte {
	if len(s.carry) == 0 {
		return chunk
	}
	b := slices.Concat(s.carry, chunk)
	s.carry = nil
	return b
}

// pieceLen returns how many leading bytes of b form the next piece: at most
// maxOutputLine, never ending inside a multi-byte UTF-8 sequence. A piece that
// reaches the end of b, which only happens while more of the line may follow,
// also leaves out an incomplete trailing sequence and a trailing '\r', which
// belong with the bytes still to come. It never returns 0 for a non-empty b,
// so invalid UTF-8 cannot stall the loop.
func pieceLen(b []byte) int {
	n := min(len(b), maxOutputLine)
	if n < len(b) {
		// Cutting before b[n]: back off over at most UTFMax-1 continuation bytes.
		for k := 0; k < utf8.UTFMax-1 && n > 0 && !utf8.RuneStart(b[n]); k++ {
			n--
		}
	} else {
		if i := lastRuneStart(b); i >= 0 && !utf8.FullRune(b[i:]) {
			n = i
		}
		if n > 0 && n == len(b) && b[n-1] == '\r' {
			n--
		}
	}
	if n == 0 {
		n = min(len(b), maxOutputLine)
	}
	return n
}

// lastRuneStart returns the index of the last byte in b that can start a
// UTF-8 sequence, looking back at most UTFMax bytes, or -1.
func lastRuneStart(b []byte) int {
	for i := len(b) - 1; i >= 0 && i >= len(b)-utf8.UTFMax; i-- {
		if utf8.RuneStart(b[i]) {
			return i
		}
	}
	return -1
}
