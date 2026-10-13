package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"sort"

	"github.com/lechefran/mailban.workers/internal/logchain"
)

// chainResult is the outcome of spec verification step 2.
type chainResult struct {
	Rows      int
	Chained   int
	Unchained int
	// LateUnchained counts unchained rows below the highest chained seq:
	// late commits the workers could not chain. They are covered by no
	// checkpoint but are still replayed.
	LateUnchained int
	// Seqs of failing rows and checkpoints.
	LeafFailures      []int64
	ChainFailures     []int64
	SignatureFailures []int64
	// CheckpointMismatches are checkpoints whose chain differs from the log's
	// chain at that seq, or whose seq is not a chained row.
	CheckpointMismatches []int64
	CheckpointsVerified  int
	// NoKey is set when the API publishes no signing key.
	NoKey bool
}

// failed reports whether any integrity check failed.
func (r chainResult) failed() bool {
	return len(r.LeafFailures)+len(r.ChainFailures)+len(r.SignatureFailures)+len(r.CheckpointMismatches) > 0
}

// verifyChain recomputes leaves and chains of the chained rows and checks the
// checkpoints (spec "Verifying", step 2). events must be in ascending seq
// order. Redacted rows' leaves are taken as stored, since redaction changes
// the fields they were computed from. Each chain is recomputed from the
// previous stored chain, so one tampered row yields one failure.
func verifyChain(events []event, cps []checkpoint, pub ed25519.PublicKey) chainResult {
	res := chainResult{Rows: len(events), NoKey: pub == nil}
	chainAt := make(map[int64]string)
	prev := logchain.Genesis
	var head int64
	for _, e := range events {
		if e.Chain == nil {
			res.Unchained++
			continue
		}
		res.Chained++
		if e.Leaf == nil {
			res.LeafFailures = append(res.LeafFailures, e.Seq)
			res.ChainFailures = append(res.ChainFailures, e.Seq)
			prev, head = *e.Chain, e.Seq
			chainAt[e.Seq] = *e.Chain
			continue
		}
		if !e.Redacted && logchain.Leaf(e.row()) != *e.Leaf {
			res.LeafFailures = append(res.LeafFailures, e.Seq)
		}
		if logchain.Chain(prev, *e.Leaf) != *e.Chain {
			res.ChainFailures = append(res.ChainFailures, e.Seq)
		}
		prev, head = *e.Chain, e.Seq
		chainAt[e.Seq] = *e.Chain
	}
	for _, e := range events {
		if e.Chain == nil && e.Seq < head {
			res.LateUnchained++
		}
	}

	for _, cp := range cps {
		sig, err := base64.StdEncoding.DecodeString(cp.Signature)
		if pub == nil || err != nil || !ed25519.Verify(pub, logchain.CheckpointMessage(cp.Seq, cp.Chain), sig) {
			res.SignatureFailures = append(res.SignatureFailures, cp.Seq)
			continue
		}
		if chain, ok := chainAt[cp.Seq]; !ok || chain != cp.Chain {
			res.CheckpointMismatches = append(res.CheckpointMismatches, cp.Seq)
			continue
		}
		res.CheckpointsVerified++
	}
	return res
}

// row converts an API event to the leaf's fields.
func (e event) row() logchain.Row {
	return logchain.Row{Seq: e.Seq, At: e.At, Action: e.Action,
		AddressID: e.AddressID, MailboxID: e.MailboxID, VoterID: e.VoterID, Reputable: e.Reputable}
}

// counts is an address's vote tally.
type counts struct {
	Positive int
	Total    int
}

// replay rebuilds per-address tallies from the log (spec step 3 and 4).
// Rows with a null address_id (opt-outs and redacted rows) are skipped;
// cast and change set the voter's vote, retract, expire and suspend delete it.
func replay(events []event) map[string]counts {
	votes := make(map[string]map[string]bool)
	for _, e := range events {
		if e.AddressID == nil {
			continue
		}
		voter := ""
		if e.VoterID != nil {
			voter = *e.VoterID
		}
		switch e.Action {
		case "cast", "change":
			if e.Reputable == nil {
				continue
			}
			if votes[*e.AddressID] == nil {
				votes[*e.AddressID] = make(map[string]bool)
			}
			votes[*e.AddressID][voter] = *e.Reputable
		case "retract", "expire", "suspend":
			delete(votes[*e.AddressID], voter)
		}
	}

	out := make(map[string]counts, len(votes))
	for addr, byVoter := range votes {
		var c counts
		for _, reputable := range byVoter {
			c.Total++
			if reputable {
				c.Positive++
			}
		}
		if c.Total > 0 {
			out[addr] = c
		}
	}
	return out
}

// scoreMismatch is an address whose replayed tally differs from the API.
type scoreMismatch struct {
	// Address is the plain address when the API lists it, else "id:<address_id>".
	Address string
	Log     counts
	API     counts
}

// compareScores compares replayed tallies with the API's addresses, mapped to
// sha256_hex(address). Addresses the API lists with zero votes count as
// absent. Mismatches are sorted by Address.
func compareScores(replayed map[string]counts, emails []email) []scoreMismatch {
	var out []scoreMismatch
	seen := make(map[string]bool, len(emails))
	for _, em := range emails {
		id := sha256Hex(em.Address)
		seen[id] = true
		api := counts{Positive: em.PositiveVotes, Total: em.TotalVotes}
		if got := replayed[id]; got != api {
			out = append(out, scoreMismatch{Address: em.Address, Log: got, API: api})
		}
	}
	for id, c := range replayed {
		if !seen[id] {
			out = append(out, scoreMismatch{Address: "id:" + id, Log: c})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Address < out[j].Address })
	return out
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
