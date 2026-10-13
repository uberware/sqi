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
	h    hash.Hash
	rows int64
}

func newDigest() *digest { return &digest{h: sha256.New()} }

func (d *digest) row(vals []any) {
	d.rows++
	d.write("R" + strconv.Itoa(len(vals)))
	for _, v := range vals {
		d.write(token(v))
	}
}

func token(v any) string {
	switch x := v.(type) {
	case nil:
		return "N"
	case int64:
		return "I" + strconv.FormatInt(x, 10)
	case bool:
		if x {
			return "I1"
		}
		return "I0"
	case string:
		return "S" + x
	case []byte:
		return "S" + string(x)
	default:
		return fmt.Sprintf("?%T:%v", v, v)
	}
}

func (d *digest) write(tok string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(tok)))
	d.h.Write(n[:])
	d.h.Write([]byte(tok))
}

// sum is the hash and the row count, for comparison and for messages.
func (d *digest) sum() string {
	return hex.EncodeToString(d.h.Sum(nil)) + "/" + strconv.FormatInt(d.rows, 10)
}
