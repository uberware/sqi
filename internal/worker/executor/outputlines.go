// SPDX-License-Identifier: AGPL-3.0-or-later

package executor

import (
	"bufio"
	"unicode/utf8"
)

// maxOutputLine is the most bytes of one task output line delivered as a
// single piece. A longer line is delivered as consecutive pieces of at most
// this many bytes. It is bufio.Scanner's default token limit, so lines that
// were delivered whole before still are.
const maxOutputLine = bufio.MaxScanTokenSize

// lineSplitter turns the slices bufio.Reader.ReadSlice returns into lines and,
// for a line longer than maxOutputLine, pieces. emit receives a fresh copy of
// each line or piece.
type lineSplitter struct {
	emit func([]byte)
	// onSplit is called each time a line is cut into pieces.
	onSplit func()
	// carry holds the bytes of a line already read but not yet delivered: an
	// incomplete trailing UTF-8 sequence, or a '\r' that may precede '\n'.
	carry []byte
	// split is true once the current line has delivered a piece.
	split bool
}

// partial takes a buffer-full of a line whose end has not been read yet.
func (s *lineSplitter) partial(chunk []byte) {
	b := s.join(chunk)
	n := pieceLen(b, maxOutputLine, true)
	s.deliverPiece(b[:n])
	s.carry = b[n:]
}

// end takes the last bytes of a line, without its '\n'. It is also called at
// EOF with whatever remains, which ends the final unterminated line.
func (s *lineSplitter) end(chunk []byte) {
	b := s.join(chunk)
	if len(b) > 0 && b[len(b)-1] == '\r' {
		b = b[:len(b)-1]
	}
	for len(b) > maxOutputLine {
		n := pieceLen(b, maxOutputLine, false)
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

// pending reports whether bytes of an unfinished line are held back.
func (s *lineSplitter) pending() bool { return len(s.carry) > 0 || s.split }

func (s *lineSplitter) deliverPiece(p []byte) {
	if !s.split && s.onSplit != nil {
		s.onSplit()
	}
	s.split = true
	s.emit(p)
}

// join returns carry followed by chunk in a new slice, so neither the reader's
// reused buffer nor carry is aliased by what is delivered.
func (s *lineSplitter) join(chunk []byte) []byte {
	b := make([]byte, 0, len(s.carry)+len(chunk))
	b = append(b, s.carry...)
	b = append(b, chunk...)
	s.carry = nil
	return b
}

// pieceLen returns how many leading bytes of b form the next piece: at most
// limit, never ending inside a multi-byte UTF-8 sequence. When more of the
// line may follow (open), a piece that reaches the end of b also leaves out an
// incomplete trailing sequence and a trailing '\r', which belong with the
// bytes still to come. It never returns 0 for a non-empty b, so invalid UTF-8
// cannot stall the loop.
func pieceLen(b []byte, limit int, open bool) int {
	n := min(len(b), limit)
	if n < len(b) {
		// Cutting before b[n]: back off over at most UTFMax-1 continuation bytes.
		for k := 0; k < utf8.UTFMax-1 && n > 0 && !utf8.RuneStart(b[n]); k++ {
			n--
		}
	} else if open {
		if i := lastRuneStart(b); i >= 0 && !utf8.FullRune(b[i:]) {
			n = i
		}
		if n > 0 && n == len(b) && b[n-1] == '\r' {
			n--
		}
	}
	if n == 0 {
		n = min(len(b), limit)
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
