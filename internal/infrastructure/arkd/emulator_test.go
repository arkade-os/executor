package arkd

import (
	"context"
	"strings"
	"testing"

	"github.com/arkade-os/arkd/pkg/ark-lib/extension"
	"github.com/arkade-os/arkd/pkg/ark-lib/script"
	"github.com/arkade-os/arkd/pkg/ark-lib/txsigner"
	"github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	"github.com/arkade-os/emulator/pkg/arkade"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

func TestSubmitTx(t *testing.T) {
	signer, other := testKey(1), testKey(2)
	covenant := []byte{arkade.OP_1}
	tweaked := arkade.ComputeArkadeScriptPrivateKey(signer, arkade.ArkadeScriptHash(covenant))
	leaf, err := (&script.MultisigClosure{
		PubKeys: []*btcec.PublicKey{signer.PubKey(), tweaked.PubKey()},
	}).Script()
	require.NoError(t, err)
	checkpoint := spend(t, wire.OutPoint{Index: 7}, leaf, nil)
	arkTx := spend(t, wire.OutPoint{Hash: checkpoint.UnsignedTx.TxHash()}, leaf, covenant)
	foreign := spend(t, wire.OutPoint{Index: 8}, leaf, nil)

	fixtures := []struct {
		name           string
		tx, checkpoint *psbt.Packet
		txKeys, cpKeys []*btcec.PrivateKey
		refused        string
	}{
		{
			name: "both of arkd's keys sign both transactions", tx: arkTx, checkpoint: checkpoint,
			txKeys: []*btcec.PrivateKey{signer, tweaked},
			cpKeys: []*btcec.PrivateKey{signer, tweaked},
		},
		{
			name: "the checkpoint misses the tweaked key", tx: arkTx, checkpoint: checkpoint,
			txKeys: []*btcec.PrivateKey{signer, tweaked}, cpKeys: []*btcec.PrivateKey{signer},
			refused: "missing signature",
		},
		{
			name: "the ark tx is unsigned", tx: arkTx, checkpoint: checkpoint,
			cpKeys: []*btcec.PrivateKey{signer, tweaked}, refused: "no tapscript signatures",
		},
		{
			name: "another key signs", tx: arkTx, checkpoint: checkpoint,
			txKeys: []*btcec.PrivateKey{signer, tweaked}, cpKeys: []*btcec.PrivateKey{other},
			refused: "missing signature",
		},
		{
			name: "another checkpoint is signed", tx: arkTx, checkpoint: foreign,
			txKeys: []*btcec.PrivateKey{signer, tweaked},
			cpKeys: []*btcec.PrivateKey{signer, tweaked}, refused: "is not among the signed ones",
		},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			ark := &cosigner{
				signedTx:          sign(t, f.tx, f.txKeys...),
				signedCheckpoints: []string{sign(t, f.checkpoint, f.cpKeys...)},
			}
			_, _, err := NewEmulator(ark).SubmitTx(
				t.Context(), encode(t, arkTx), []string{encode(t, checkpoint)},
			)
			if f.refused == "" {
				require.NoError(t, err)
				require.True(t, ark.finalized)
				return
			}
			require.ErrorContains(t, err, f.refused)
			require.False(t, ark.finalized, "nothing is finalized on signatures not verified")
		})
	}

	t.Run("a signature over another transaction", func(t *testing.T) {
		forged := sign(t, foreign, signer, tweaked)
		moved, err := psbt.NewFromRawBytes(strings.NewReader(forged), true)
		require.NoError(t, err)
		signed, err := psbt.NewFromRawBytes(strings.NewReader(encode(t, checkpoint)), true)
		require.NoError(t, err)
		signed.Inputs[0].TaprootScriptSpendSig = moved.Inputs[0].TaprootScriptSpendSig
		ark := &cosigner{
			signedTx:          sign(t, arkTx, signer, tweaked),
			signedCheckpoints: []string{encode(t, signed)},
		}
		_, _, err = NewEmulator(ark).SubmitTx(
			t.Context(), encode(t, arkTx), []string{encode(t, checkpoint)},
		)
		require.ErrorContains(t, err, "invalid sig")
		require.False(t, ark.finalized)
	})
}

func TestSubmitOnchainTx(t *testing.T) {
	funding := wire.NewMsgTx(2)
	funding.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 1}})
	funding.AddTxOut(wire.NewTxOut(10_000, []byte{0x51}))
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Hash: funding.TxHash()}})
	tx.AddTxOut(wire.NewTxOut(9_000, []byte{0x51}))
	ptx, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	taptree := txutils.TapTree{"51"}
	require.NoError(t, txutils.SetArkPsbtField(ptx, 0, txutils.VtxoTaprootTreeField, taptree))
	require.NoError(t, txutils.SetArkPsbtField(ptx, 0, arkade.PrevoutTxField, *funding))
	encoded, err := ptx.B64Encode()
	require.NoError(t, err)

	ark := &cosigner{}
	signed, broadcast, err := NewEmulator(ark).SubmitOnchainTx(t.Context(), encoded)
	require.NoError(t, err)
	require.Empty(t, signed)
	require.True(t, broadcast, "arkd broadcasts what it cosigns")

	cosigned, err := psbt.NewFromRawBytes(strings.NewReader(ark.tx), true)
	require.NoError(t, err)
	require.Equal(t, tx.TxHash(), cosigned.UnsignedTx.TxHash())
	prevoutTxs, err := txutils.GetArkPsbtFields(cosigned, 0, arkade.PrevoutTxField)
	require.NoError(t, err)
	require.Empty(t, prevoutTxs, "arkd attaches the funding transaction itself")
	taptrees, err := txutils.GetArkPsbtFields(cosigned, 0, txutils.VtxoTaprootTreeField)
	require.NoError(t, err)
	require.Equal(t, []txutils.TapTree{taptree}, taptrees)
}

type cosigner struct {
	ports.Signer
	tx                string
	signedTx          string
	signedCheckpoints []string
	finalized         bool
}

func (c *cosigner) CosignOnchainTx(_ context.Context, tx string) (string, error) {
	c.tx = tx
	return "", nil
}

func (c *cosigner) SubmitTx(
	context.Context, string, []string,
) (string, string, []string, error) {
	return "txid", c.signedTx, c.signedCheckpoints, nil
}

func (c *cosigner) FinalizeTx(context.Context, string, []string) error {
	c.finalized = true
	return nil
}

func spend(t *testing.T, prevout wire.OutPoint, leaf, covenant []byte) *psbt.Packet {
	t.Helper()
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: prevout})
	tx.AddTxOut(wire.NewTxOut(10_000, []byte{0x51}))
	if covenant != nil {
		packet, err := arkade.NewPacket(arkade.EmulatorEntry{Vin: 0, Script: covenant})
		require.NoError(t, err)
		out, err := extension.Extension{packet}.TxOut()
		require.NoError(t, err)
		tx.AddTxOut(out)
	}
	ptx, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	closure, err := script.DecodeClosure(leaf)
	require.NoError(t, err)
	vtxoScript := script.TapscriptsVtxoScript{Closures: []script.Closure{closure}}
	tapKey, taptree, err := vtxoScript.TapTree()
	require.NoError(t, err)
	pkScript, err := script.P2TRScript(tapKey)
	require.NoError(t, err)
	proof, err := taptree.GetTaprootMerkleProof(txscript.NewBaseTapLeaf(leaf).TapHash())
	require.NoError(t, err)
	ptx.Inputs[0].WitnessUtxo = wire.NewTxOut(10_000, pkScript)
	ptx.Inputs[0].TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
		ControlBlock: proof.ControlBlock, Script: leaf, LeafVersion: txscript.BaseLeafVersion,
	}}
	return ptx
}

func sign(t *testing.T, tx *psbt.Packet, keys ...*btcec.PrivateKey) string {
	t.Helper()
	ptx, err := psbt.NewFromRawBytes(strings.NewReader(encode(t, tx)), true)
	require.NoError(t, err)
	fetcher, err := txutils.GetPrevOutputFetcher(ptx)
	require.NoError(t, err)
	hashes := txscript.NewTxSigHashes(ptx.UnsignedTx, fetcher)
	for _, key := range keys {
		require.NoError(t, txsigner.SignTapscriptInput(ptx, 0, key, hashes))
	}
	return encode(t, ptx)
}

func encode(t *testing.T, tx *psbt.Packet) string {
	t.Helper()
	encoded, err := tx.B64Encode()
	require.NoError(t, err)
	return encoded
}

func testKey(scalar uint32) *btcec.PrivateKey {
	var s btcec.ModNScalar
	s.SetInt(scalar)
	return btcec.PrivKeyFromScalar(&s)
}
