package logchain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func ptr[T any](v T) *T { return &v }

// The expected hashes below were computed outside Go with
// printf '<bytes>' | sha256sum, so they pin the byte format, not this code.
var (
	castRow = Row{Seq: 41, At: 1760270000123456, Action: "cast",
		AddressID: ptr("aa11"), MailboxID: ptr("bb22"), VoterID: ptr("cc33"), Reputable: ptr(true)}
	optoutRow = Row{Seq: 42, At: 1760270000123457, Action: "optout"}
)

const (
	castLeaf    = "180dd976b783acec3d814b4a8d820ca42180641d7c8995db57283dc74e1c3fb3"
	castChain   = "a81f0b492f019e0d54a5b5f645d2de3acca44d39a2c6c11c526186f6cac399d1"
	optoutLeaf  = "516e5791837bcd111e9df0528c580af52a66e5e62a6b7d8122c64270952adce8"
	optoutChain = "b09de374dd8a8060e447661b9e01b68eeabe130f5c62ab735f2d30261d55e982"
)

func TestLeafBytes(t *testing.T) {
	assert.Equal(t,
		`{"seq":41,"at":1760270000123456,"action":"cast","address_id":"aa11","mailbox_id":"bb22","voter_id":"cc33","reputable":true}`,
		string(LeafBytes(castRow)))
	assert.Equal(t,
		`{"seq":42,"at":1760270000123457,"action":"optout","address_id":null,"mailbox_id":null,"voter_id":null,"reputable":null}`,
		string(LeafBytes(optoutRow)))

	r := castRow
	r.Reputable = ptr(false)
	assert.Contains(t, string(LeafBytes(r)), `"reputable":false}`)
}

func TestLeafBytesEscapesUnexpectedCharacters(t *testing.T) {
	r := Row{Seq: 1, At: 2, Action: "cast", VoterID: ptr("a\"b\\c\n")}
	assert.Contains(t, string(LeafBytes(r)), `"voter_id":"a\"b\\c\u000a"`)
}

func TestKnownVector(t *testing.T) {
	assert.Len(t, Genesis, 64)
	assert.Equal(t, castLeaf, Leaf(castRow))
	assert.Equal(t, castChain, Chain(Genesis, castLeaf))
	assert.Equal(t, optoutLeaf, Leaf(optoutRow))
	assert.Equal(t, optoutChain, Chain(castChain, optoutLeaf))
}

func TestCheckpointMessage(t *testing.T) {
	assert.Equal(t, "mailban-checkpoint-v1:42:"+optoutChain, string(CheckpointMessage(42, optoutChain)))
}
