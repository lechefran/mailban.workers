// Package logchain computes the vote log's leaf and chain hashes and the
// checkpoint message exactly as docs/reputation-spec.md (mailban2) defines
// them. It is shared by the chain and checkpoint jobs and by cmd/verify, so
// the writer and every verifier hash the same bytes.
package logchain

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// Genesis is the previous chain value of the first chained row: 64 ASCII zeros.
const Genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Row holds the fields of one vote_log row that the leaf covers. Nil pointers
// are SQL NULLs and are written as JSON null.
type Row struct {
	Seq       int64
	At        int64 // microseconds since the Unix epoch
	Action    string
	AddressID *string
	MailboxID *string
	VoterID   *string
	Reputable *bool
}

// LeafBytes returns the exact byte string L whose SHA-256 is the row's leaf:
// a JSON object with no spaces and the keys in the spec's fixed order.
func LeafBytes(r Row) []byte {
	var b strings.Builder
	b.Grow(256)
	b.WriteString(`{"seq":`)
	b.WriteString(strconv.FormatInt(r.Seq, 10))
	b.WriteString(`,"at":`)
	b.WriteString(strconv.FormatInt(r.At, 10))
	b.WriteString(`,"action":`)
	writeString(&b, &r.Action)
	b.WriteString(`,"address_id":`)
	writeString(&b, r.AddressID)
	b.WriteString(`,"mailbox_id":`)
	writeString(&b, r.MailboxID)
	b.WriteString(`,"voter_id":`)
	writeString(&b, r.VoterID)
	b.WriteString(`,"reputable":`)
	switch {
	case r.Reputable == nil:
		b.WriteString("null")
	case *r.Reputable:
		b.WriteString("true")
	default:
		b.WriteString("false")
	}
	b.WriteByte('}')
	return []byte(b.String())
}

// Leaf returns hex(sha256(LeafBytes(r))).
func Leaf(r Row) string {
	sum := sha256.Sum256(LeafBytes(r))
	return hex.EncodeToString(sum[:])
}

// Chain returns hex(sha256(prev + leaf)), where prev is the previous chained
// row's chain (or Genesis) and both are ASCII hex strings.
func Chain(prev, leaf string) string {
	sum := sha256.Sum256([]byte(prev + leaf))
	return hex.EncodeToString(sum[:])
}

// CheckpointMessage returns the signed checkpoint message
// "mailban-checkpoint-v1:<seq>:<chain>".
func CheckpointMessage(seq int64, chain string) []byte {
	return []byte("mailban-checkpoint-v1:" + strconv.FormatInt(seq, 10) + ":" + chain)
}

// writeString writes s as a JSON string, or null when s is nil. Log values are
// ASCII hex digests, fixed action names and "legacy-"/"erased-" pseudonyms, so
// in practice nothing is escaped; quotes, backslashes and control characters
// are escaped per RFC 8259 so the bytes stay valid JSON whatever is stored.
func writeString(b *strings.Builder, s *string) {
	if s == nil {
		b.WriteString("null")
		return
	}
	b.WriteByte('"')
	for i := 0; i < len(*s); i++ {
		c := (*s)[i]
		switch {
		case c == '"' || c == '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20:
			b.WriteString(`\u00`)
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0xf])
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

const hexDigits = "0123456789abcdef"
