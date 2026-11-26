package execution

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/arkade-os/arkd/pkg/ark-lib/asset"
	"github.com/arkade-os/arkd/pkg/ark-lib/extension"
	"github.com/arkade-os/arkd/pkg/ark-lib/tree"
	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/internal/core/domain/execution"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/chainhash/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

func TestStarted(t *testing.T) {
	floor, session := 1024*time.Second, time.Minute
	for _, tc := range []struct {
		name      string
		expiry    arklib.RelativeLocktime
		confirmed bool
	}{
		{name: "seconds below the floor", expiry: seconds(512)},
		{name: "blocks below the floor", expiry: blocks(1)},
		{name: "seconds at the floor", expiry: seconds(1024), confirmed: true},
		{name: "blocks above the floor", expiry: blocks(2), confirmed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ark := &countingSigner{}
			s := &Service{
				ark: ark, batchCosigner: "batchCosigner", ctx: t.Context(),
				events: make(chan any, 8), slots: make(chan struct{}, 2),
				limits:  Limits{MinBatchExpiry: floor},
				session: execution.Session{Duration: session},
			}
			b := newBatchWorker(s)
			for i, txid := range []string{"a", "b"} {
				b.intents[txid] = &registration{attempt: delegation.Attempt{
					DelegationID: int64(i + 1), Coins: []string{txid + ":0"},
					Txs: []delegation.Tx{{Txid: txid}},
				}}
			}

			b.started(ports.BatchStarted{
				ID: "batch", Selected: []string{"a", "b", "theirs"}, BatchExpiry: tc.expiry,
			})

			var failures []attemptFailed
			var taken []int64
			for range len(s.events) {
				switch ev := (<-s.events).(type) {
				case attemptFailed:
					failures = append(failures, ev)
				case selected:
					taken = append(taken, ev.delegations...)
				}
			}
			require.Empty(t, b.intents)
			if tc.confirmed {
				require.EqualValues(t, 2, ark.confirmed.Load())
				require.Equal(t, [][]string{{"a:0", "b:0", "batchCosigner"}}, ark.followed)
				require.Empty(t, failures)
				require.Equal(t, []int64{1, 2}, taken)
				require.NotNil(t, b.inFlight)
				require.Equal(t, tc.expiry, b.inFlight.expiry)
				require.Len(t, b.inFlight.intents, 2)
				return
			}
			require.Zero(t, ark.confirmed.Load())
			require.Empty(t, ark.followed)
			require.Nil(t, b.inFlight)
			require.Empty(t, taken)
			require.Len(t, failures, 2)
			for i, f := range failures {
				require.Equal(t, int64(i+1), f.attempt.DelegationID)
				require.ErrorContains(t, f.err, "below the minimum")
				require.False(t, chargesTemplate(f.err))
				require.False(t, rotates(f.err))
				require.Equal(t, session, f.retryIn)
			}
		})
	}
}

func TestSignatures(t *testing.T) {
	t.Run("signs once per batch", func(t *testing.T) {
		session := &countingSession{}
		ark := &countingSigner{}
		b := &batchWorker{
			s:        &Service{ark: ark, batchCosigner: "batchCosigner"},
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

func TestLifetimeOf(t *testing.T) {
	floor := time.Hour
	for _, tc := range []struct {
		name     string
		expiry   arklib.RelativeLocktime
		expected time.Duration
		below    bool
	}{
		{
			name: "seconds below the floor", expiry: seconds(3599),
			expected: 3599 * time.Second, below: true,
		},
		{name: "seconds at the floor", expiry: seconds(3600), expected: time.Hour},
		{
			name: "blocks below the floor", expiry: blocks(5),
			expected: 50 * time.Minute, below: true,
		},
		{name: "blocks at the floor", expiry: blocks(6), expected: time.Hour},
		{name: "blocks above the floor", expiry: blocks(144), expected: 24 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lifetime := lifetimeOf(tc.expiry)

			require.Equal(t, tc.expected, lifetime)
			require.Equal(t, tc.below, lifetime < floor)
		})
	}
}

func TestDelivers(t *testing.T) {
	out := func(value int64, script string, assets ...uint64) delivery {
		d := delivery{out: &wire.TxOut{Value: value, PkScript: []byte(script)}}
		for i, amount := range assets {
			if d.assets == nil {
				d.assets = map[string]uint64{}
			}
			d.assets[string(rune('A'+i))] = amount
		}
		return d
	}
	for _, tc := range []struct {
		name      string
		delivered []delivery
		wanted    []delivery
		expected  bool
	}{
		{
			name:      "all outputs delivered next to others",
			delivered: []delivery{out(1, "a"), out(2, "b"), out(0, "anchor")},
			wanted:    []delivery{out(2, "b"), out(1, "a")},
			expected:  true,
		},
		{
			name:      "nothing wanted",
			delivered: []delivery{out(1, "a")},
			expected:  true,
		},
		{
			name:      "output missing",
			delivered: []delivery{out(1, "a")},
			wanted:    []delivery{out(1, "a"), out(2, "b")},
		},
		{
			name:      "wrong amount",
			delivered: []delivery{out(1, "a")},
			wanted:    []delivery{out(2, "a")},
		},
		{
			name:      "wrong script",
			delivered: []delivery{out(1, "a")},
			wanted:    []delivery{out(1, "b")},
		},
		{
			name:      "one output cannot serve two",
			delivered: []delivery{out(1, "a")},
			wanted:    []delivery{out(1, "a"), out(1, "a")},
		},
		{
			name:      "assets delivered",
			delivered: []delivery{out(1, "a", 7, 3)},
			wanted:    []delivery{out(1, "a", 7, 3)},
			expected:  true,
		},
		{
			name:      "asset missing",
			delivered: []delivery{out(1, "a", 7)},
			wanted:    []delivery{out(1, "a", 7, 3)},
		},
		{
			name:      "wrong asset amount",
			delivered: []delivery{out(1, "a", 6)},
			wanted:    []delivery{out(1, "a", 7)},
		},
		{
			name:      "the output wanting assets chooses before its twin",
			delivered: []delivery{out(1, "a", 7), out(1, "a")},
			wanted:    []delivery{out(1, "a"), out(1, "a", 7)},
			expected:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.expected, delivers(tc.delivered, tc.wanted))
		})
	}
}

func TestCheckTree(t *testing.T) {
	ours, theirs, forfeit := testKey(3), testKey(4), testKey(5).PubKey()
	expiry := arklib.RelativeLocktime{Type: arklib.LocktimeTypeSecond, Value: 1024}
	vtxo, stranger, exit := payTo(t, 10_000, 6), payTo(t, 20_000, 7), payTo(t, 5_000, 8)
	held, fewer := assetPacket(t, 7), assetPacket(t, 6)
	for _, tc := range []struct {
		name     string
		intent   *registration
		leaves   []tree.Leaf
		paid     []*wire.TxOut
		expected string
	}{
		{
			name:   "outputs delivered under the batch cosigner key",
			intent: intentOf(t, []*wire.TxOut{vtxo}, nil, nil),
			leaves: []tree.Leaf{leafOf(t, ours, nil, vtxo), leafOf(t, theirs, nil, stranger)},
		},
		{
			name:     "output under another cosigner",
			intent:   intentOf(t, []*wire.TxOut{vtxo}, nil, nil),
			leaves:   []tree.Leaf{leafOf(t, theirs, nil, vtxo), leafOf(t, ours, nil, stranger)},
			expected: "under the batch cosigner key",
		},
		{
			name:   "onchain output paid",
			intent: intentOf(t, []*wire.TxOut{vtxo}, nil, []*wire.TxOut{exit}),
			leaves: []tree.Leaf{leafOf(t, ours, nil, vtxo)},
			paid:   []*wire.TxOut{exit},
		},
		{
			name:     "onchain output missing",
			intent:   intentOf(t, []*wire.TxOut{vtxo}, nil, []*wire.TxOut{exit}),
			leaves:   []tree.Leaf{leafOf(t, ours, nil, vtxo)},
			expected: "onchain outputs",
		},
		{
			name:     "onchain output paid short",
			intent:   intentOf(t, []*wire.TxOut{vtxo}, nil, []*wire.TxOut{exit}),
			leaves:   []tree.Leaf{leafOf(t, ours, nil, vtxo)},
			paid:     []*wire.TxOut{wire.NewTxOut(exit.Value-1, exit.PkScript)},
			expected: "onchain outputs",
		},
		{
			name:   "assets delivered",
			intent: intentOf(t, []*wire.TxOut{vtxo}, held, nil),
			leaves: []tree.Leaf{leafOf(t, ours, held, vtxo)},
		},
		{
			name:     "assets missing",
			intent:   intentOf(t, []*wire.TxOut{vtxo}, held, nil),
			leaves:   []tree.Leaf{leafOf(t, ours, nil, vtxo)},
			expected: "under the batch cosigner key",
		},
		{
			name:     "fewer assets than registered",
			intent:   intentOf(t, []*wire.TxOut{vtxo}, held, nil),
			leaves:   []tree.Leaf{leafOf(t, ours, fewer, vtxo)},
			expected: "under the batch cosigner key",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := sweepTapTreeRoot(forfeit, expiry)
			require.NoError(t, err)
			batchScript, batchAmount, err := tree.BuildBatchOutput(tc.leaves, root)
			require.NoError(t, err)
			tx := wire.NewMsgTx(2)
			tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 7}, nil, nil))
			tx.AddTxOut(wire.NewTxOut(batchAmount, batchScript))
			for _, out := range tc.paid {
				tx.AddTxOut(out)
			}
			commitment, err := psbt.NewFromUnsignedTx(tx)
			require.NoError(t, err)
			vtxoTree, err := tree.BuildVtxoTree(
				&wire.OutPoint{Hash: tx.TxHash()}, tc.leaves, root, expiry,
			)
			require.NoError(t, err)
			b := &batchWorker{
				s: &Service{info: ports.ArkInfo{ForfeitPubkey: forfeit}, batchCosignerKey: ours},
			}
			r := &current{expiry: expiry, intents: []*registration{tc.intent}}

			err = b.checkTree(r, commitment, vtxoTree)
			if tc.expected == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tc.expected)
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
	confirmed atomic.Int32
	followed  [][]string
}

func (s *countingSigner) ConfirmRegistration(context.Context, string) error {
	s.confirmed.Add(1)
	return nil
}

func (s *countingSigner) UpdateTopics(_ context.Context, add, _ []string) error {
	s.followed = append(s.followed, add)
	return nil
}

func (s *countingSigner) SubmitTreeSignatures(
	context.Context, string, string, tree.TreePartialSigs,
) error {
	s.submitted++
	return nil
}

func seconds(value uint32) arklib.RelativeLocktime {
	return arklib.RelativeLocktime{Type: arklib.LocktimeTypeSecond, Value: value}
}

func blocks(value uint32) arklib.RelativeLocktime {
	return arklib.RelativeLocktime{Type: arklib.LocktimeTypeBlock, Value: value}
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

func payTo(t *testing.T, value int64, scalar uint32) *wire.TxOut {
	t.Helper()
	script, err := txscript.PayToTaprootScript(testKey(scalar).PubKey())
	require.NoError(t, err)
	return wire.NewTxOut(value, script)
}

func assetPacket(t *testing.T, amount uint64) asset.Packet {
	t.Helper()
	id, err := asset.NewAssetId(strings.Repeat("ab", 32), 0)
	require.NoError(t, err)
	in, err := asset.NewAssetInput(0, amount)
	require.NoError(t, err)
	out, err := asset.NewAssetOutput(0, amount)
	require.NoError(t, err)
	group, err := asset.NewAssetGroup(
		id, nil, []asset.AssetInput{*in}, []asset.AssetOutput{*out}, nil,
	)
	require.NoError(t, err)
	packet, err := asset.NewPacket([]asset.AssetGroup{*group})
	require.NoError(t, err)
	return packet
}

func intentOf(
	t *testing.T, offchain []*wire.TxOut, assets asset.Packet, onchain []*wire.TxOut,
) *registration {
	t.Helper()
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Index: 1}, nil, nil))
	for _, out := range offchain {
		tx.AddTxOut(out)
	}
	if assets != nil {
		ext, err := extension.Extension{assets}.TxOut()
		require.NoError(t, err)
		tx.AddTxOut(ext)
	}
	for _, out := range onchain {
		tx.AddTxOut(out)
	}
	ptx, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	return &registration{intent: &execution.Intent{
		Tx: ptx, Outputs: tx.TxOut[:len(offchain)], Onchain: onchain,
	}}
}

func leafOf(
	t *testing.T, batchCosigner *btcec.PrivateKey, assets asset.Packet, outputs ...*wire.TxOut,
) tree.Leaf {
	t.Helper()
	leaf := tree.Leaf{
		CosignersPublicKeys: []string{
			hex.EncodeToString(batchCosigner.PubKey().SerializeCompressed()),
		},
	}
	for _, out := range outputs {
		leaf.Outputs = append(leaf.Outputs, tree.LeafOutput{
			Amount: uint64(out.Value), Script: hex.EncodeToString(out.PkScript),
		})
	}
	if assets != nil {
		ext, err := extension.Extension{assets.LeafTxPacket(chainhash.Hash{1})}.Serialize()
		require.NoError(t, err)
		leaf.Outputs = append(leaf.Outputs, tree.LeafOutput{Script: hex.EncodeToString(ext)})
	}
	return leaf
}
