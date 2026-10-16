// SPDX-License-Identifier: AGPL-3.0-or-later

package storecopy

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"strconv"
)

// digest hashes rows in a form both backends produce identically. Each row is
// its value count followed by its values; each value becomes a tagged token
// (N for NULL, I for an integer or a boolean as 0/1, S for text), and every
// count and token is length-prefixed, so neither row nor field boundaries can
// collide. A value of any other Go type gets a token naming its type, which
// the other backend never produces, so it fails verification rather than
// matching by accident.
type digest struct {
	h       hash.Hash
	rows    int64
	scratch [20]byte // formats an integer token without allocating
}

func newDigest() *digest { return &digest{h: sha256.New()} }

func (d *digest) row(vals []any) {
	d.rows++
	d.write('R', strconv.AppendInt(d.scratch[:0], int64(len(vals)), 10))
	for _, v := range vals {
		d.value(v)
	}
}

func (d *digest) value(v any) {
	switch x := v.(type) {
	case nil:
		d.write('N', nil)
	case int64:
		d.write('I', strconv.AppendInt(d.scratch[:0], x, 10))
	case bool:
		if x {
			d.write('I', []byte("1"))
		} else {
			d.write('I', []byte("0"))
		}
	case string:
		d.write('S', []byte(x))
	case []byte:
		d.write('S', x)
	default:
		d.write('?', []byte(fmt.Sprintf("%T:%v", v, v)))
	}
}

// write hashes one token: its length, its tag, then its body, straight into
// the hash so a value is never concatenated with its tag first.
func (d *digest) write(tag byte, body []byte) {
	var n [9]byte
	binary.BigEndian.PutUint64(n[:8], uint64(1+len(body)))
	n[8] = tag
	d.h.Write(n[:])
	d.h.Write(body)
}

// sum is the hash and the row count, for comparison and for messages.
func (d *digest) sum() string {
	return hex.EncodeToString(d.h.Sum(nil)) + "/" + strconv.FormatInt(d.rows, 10)
}
