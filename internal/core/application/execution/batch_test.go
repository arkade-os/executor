package execution

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/arkade-os/arkd/pkg/ark-lib/tree"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

func TestSignatures(t *testing.T) {
	t.Run("signs once per batch", func(t *testing.T) {
		session := &countingSession{}
		ark := &countingSigner{}
		b := &batchWorker{
			s:        &Service{ark: ark, cosigner: "cosigner"},
			inFlight: &current{id: "batch", session: session},
		}
		ev := ports.TreeNoncesAggregated{ID: "batch"}

		b.signatures(ev)
		b.signatures(ev)

		require.Equal(t, 1, session.signs)
		require.Equal(t, 1, ark.submitted)
	})
}

func TestCheckFinalization(t *testing.T) {
	commitment := encodedPacket(t, 7)
	signed := commitmentTxid(t, commitment)
	child := func(parent string) *tree.TxTree {
		p, err := psbt.NewFromRawBytes(strings.NewReader(parent), true)
		require.NoError(t, err)
		tx := wire.NewMsgTx(2)
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: p.UnsignedTx.TxHash()}})
		tx.AddTxOut(wire.NewTxOut(1, []byte{0x51}))
		packet, err := psbt.NewFromUnsignedTx(tx)
		require.NoError(t, err)
		return &tree.TxTree{Root: packet}
	}

	for _, tc := range []struct {
		name     string
		signed   string
		event    ports.BatchFinalization
		expected string
	}{
		{
			name:   "same commitment and rooted connectors",
			signed: signed,
			event: ports.BatchFinalization{
				CommitmentTx: commitment, ConnectorTree: child(commitment),
			},
		},
		{
			name:   "no connectors",
			signed: signed,
			event:  ports.BatchFinalization{CommitmentTx: commitment},
		},
		{
			name:     "another commitment",
			signed:   signed,
			event:    ports.BatchFinalization{CommitmentTx: encodedPacket(t, 8)},
			expected: "differs",
		},
		{
			name:   "connectors of another commitment",
			signed: signed,
			event: ports.BatchFinalization{
				CommitmentTx: commitment, ConnectorTree: child(encodedPacket(t, 8)),
			},
			expected: "not rooted",
		},
		{
			name:     "no commitment",
			signed:   signed,
			event:    ports.BatchFinalization{},
			expected: "no commitment",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := (&current{commitment: tc.signed}).checkFinalization(tc.event)
			if tc.expected == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.expected)
		})
	}
}

func TestDelivers(t *testing.T) {
	out := func(value int64, script string) *wire.TxOut {
		return &wire.TxOut{Value: value, PkScript: []byte(script)}
	}
	for _, tc := range []struct {
		name      string
		delivered []*wire.TxOut
		wanted    []*wire.TxOut
		expected  bool
	}{
		{
			name:      "all outputs delivered next to others",
			delivered: []*wire.TxOut{out(1, "a"), out(2, "b"), out(0, "anchor")},
			wanted:    []*wire.TxOut{out(2, "b"), out(1, "a")},
			expected:  true,
		},
		{
			name:      "nothing wanted",
			delivered: []*wire.TxOut{out(1, "a")},
			expected:  true,
		},
		{
			name:      "output missing",
			delivered: []*wire.TxOut{out(1, "a")},
			wanted:    []*wire.TxOut{out(1, "a"), out(2, "b")},
		},
		{
			name:      "wrong amount",
			delivered: []*wire.TxOut{out(1, "a")},
			wanted:    []*wire.TxOut{out(2, "a")},
		},
		{
			name:      "wrong script",
			delivered: []*wire.TxOut{out(1, "a")},
			wanted:    []*wire.TxOut{out(1, "b")},
		},
		{
			name:      "one output cannot serve two",
			delivered: []*wire.TxOut{out(1, "a")},
			wanted:    []*wire.TxOut{out(1, "a"), out(1, "a")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, delivers(tc.delivered, tc.wanted))
		})
	}
}

func TestSameProofTx(t *testing.T) {
	proof := encodedPacket(t, 1)
	signed := encodedPacket(t, 1, bytes.Repeat([]byte{1}, 64))

	for _, tc := range []struct {
		name          string
		local, remote string
		err           string
	}{
		{name: "same tx with a signature added", local: proof, remote: signed},
		{name: "different tx", local: proof, remote: encodedPacket(t, 2), err: "differs"},
		{name: "remote not a psbt", local: proof, remote: "nope", err: "intent proof"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := sameProofTx(tc.local, tc.remote)
			if tc.err == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.err)
		})
	}
}

type countingSession struct {
	tree.SignerSession
	signs int
}

func (s *countingSession) SetAggregatedNonces(tree.TreeNonces) {}

func (s *countingSession) Sign() (tree.TreePartialSigs, error) {
	s.signs++
	return tree.TreePartialSigs{}, nil
}

type countingSigner struct {
	ports.Signer
	submitted int
}

func (s *countingSigner) SubmitTreeSignatures(
	context.Context, string, string, tree.TreePartialSigs,
) error {
	s.submitted++
	return nil
}

func commitmentTxid(t *testing.T, encoded string) string {
	t.Helper()
	p, err := psbt.NewFromRawBytes(strings.NewReader(encoded), true)
	require.NoError(t, err)
	return p.UnsignedTx.TxHash().String()
}

func encodedPacket(t *testing.T, vout uint32, witness ...[]byte) string {
	t.Helper()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: vout}, nil, nil))
	tx.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
	p, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	if len(witness) > 0 {
		p.Inputs[0].TaprootKeySpendSig = witness[0]
	}
	encoded, err := p.B64Encode()
	require.NoError(t, err)
	return encoded
}
