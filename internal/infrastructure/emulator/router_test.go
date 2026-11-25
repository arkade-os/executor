package emulator

import (
	"context"
	"testing"

	"github.com/arkade-os/arkd/pkg/ark-lib/extension"
	"github.com/arkade-os/arkd/pkg/ark-lib/script"
	"github.com/arkade-os/emulator/pkg/arkade"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

func TestSubmitOnchainTx(t *testing.T) {
	signerKey, emulatorKey := testKey(1).PubKey(), testKey(2).PubKey()
	covenant := []byte{arkade.OP_1}
	tweaked := func(key *btcec.PublicKey) *btcec.PublicKey {
		return arkade.ComputeArkadeScriptPublicKey(key, arkade.ArkadeScriptHash(covenant))
	}
	fixtures := []struct {
		name       string
		leaf       []*btcec.PublicKey
		standalone bool
	}{
		{name: "arkd's tweaked key alone", leaf: []*btcec.PublicKey{tweaked(signerKey)}},
		{
			name: "arkd's key next to its tweaked key",
			leaf: []*btcec.PublicKey{signerKey, tweaked(signerKey)},
		},
		{
			name: "arkd's key next to the emulator's tweaked key", standalone: true,
			leaf: []*btcec.PublicKey{signerKey, tweaked(emulatorKey)},
		},
		{
			name: "the emulator's tweaked key alone", standalone: true,
			leaf: []*btcec.PublicKey{tweaked(emulatorKey)},
		},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			standalone, arkd := &recorder{}, &recorder{}
			router := NewRouter(standalone, emulatorKey, arkd)

			_, _, err := router.SubmitOnchainTx(t.Context(), spend(t, f.leaf, covenant))
			require.NoError(t, err)
			require.Equal(t, f.standalone, standalone.called)
			require.Equal(t, !f.standalone, arkd.called)
		})
	}
}

type recorder struct {
	ports.Emulator
	called bool
}

func (r *recorder) SubmitOnchainTx(context.Context, string) (string, bool, error) {
	r.called = true
	return "", false, nil
}

func spend(t *testing.T, leafKeys []*btcec.PublicKey, covenant []byte) string {
	t.Helper()
	leaf, err := (&script.MultisigClosure{PubKeys: leafKeys}).Script()
	require.NoError(t, err)
	packet, err := arkade.NewPacket(arkade.EmulatorEntry{Vin: 0, Script: covenant})
	require.NoError(t, err)
	packetOut, err := extension.Extension{packet}.TxOut()
	require.NoError(t, err)
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 1}})
	tx.AddTxOut(wire.NewTxOut(9_000, []byte{0x51}))
	tx.AddTxOut(packetOut)
	ptx, err := psbt.NewFromUnsignedTx(tx)
	require.NoError(t, err)
	controlBlock := append(
		[]byte{byte(txscript.BaseLeafVersion)}, schnorr.SerializePubKey(leafKeys[0])...,
	)
	ptx.Inputs[0].TaprootLeafScript = []*psbt.TaprootTapLeafScript{{
		ControlBlock: controlBlock, Script: leaf, LeafVersion: txscript.BaseLeafVersion,
	}}
	encoded, err := ptx.B64Encode()
	require.NoError(t, err)
	return encoded
}

func testKey(scalar uint32) *btcec.PrivateKey {
	var s btcec.ModNScalar
	s.SetInt(scalar)
	return btcec.PrivKeyFromScalar(&s)
}
