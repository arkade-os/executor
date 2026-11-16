package execution

import (
	"bytes"
	"encoding/hex"
	"os"
	"testing"

	"github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	"github.com/arkade-os/emulator/pkg/arkade"
	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/pkg/template"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

const onchainAmount = 10_000

type onchain struct {
	inst     *template.Instance
	inputs   []delegation.Input
	coin     delegation.Coin
	source   *wire.MsgTx
	txs      map[string]*wire.MsgTx
	emulator *btcec.PrivateKey
}

func newOnchain(t *testing.T) onchain {
	t.Helper()
	return newOnchainFrom(t, "onchain_arkade.json", nil)
}

func newOnchainFrom(t *testing.T, file string, vars map[string][]byte) onchain {
	t.Helper()
	doc, err := os.ReadFile("testdata/" + file)
	require.NoError(t, err)
	tmpl, err := template.Parse(doc, nil)
	require.NoError(t, err)
	var keys [3]*btcec.PrivateKey
	for i := range keys {
		keys[i], _ = btcec.PrivKeyFromBytes(bytes.Repeat([]byte{byte(i + 1)}, 32))
	}
	inst, err := tmpl.Instantiate(template.Context{Keys: template.Keys{
		Server: keys[0].PubKey(), Emulator: keys[1].PubKey(), Cosigner: keys[2].PubKey(),
	}, Variables: vars})
	require.NoError(t, err)
	source := wire.NewMsgTx(3)
	source.AddTxIn(&wire.TxIn{PreviousOutPoint: wire.OutPoint{Index: 1}})
	source.AddTxOut(wire.NewTxOut(onchainAmount, inst.PkScript(0)))
	pkScript := hex.EncodeToString(inst.PkScript(0))
	return onchain{
		inst:   inst,
		inputs: []delegation.Input{{Script: pkScript, Tapscripts: inst.Tapscripts(0)}},
		coin: delegation.Coin{
			Outpoint: source.TxHash().String() + ":0", DelegationID: 1, Script: pkScript,
			Amount: onchainAmount, Onchain: true, Status: delegation.Present,
		},
		source:   source,
		txs:      map[string]*wire.MsgTx{source.TxHash().String(): source},
		emulator: keys[1],
	}
}

func (o onchain) build(t *testing.T, fee template.Fee) (*psbt.Packet, error) {
	t.Helper()
	return BuildOnchain(o.inst, o.inputs, []delegation.Coin{o.coin}, o.txs, fee)
}

// sign signs input 0 as the emulator does: with its key tweaked by the arkade script.
func (o onchain) sign(t *testing.T, ptx *psbt.Packet) {
	t.Helper()
	packet, err := arkade.FindEmulatorPacket(ptx.UnsignedTx)
	require.NoError(t, err)
	key := arkade.ComputeArkadeScriptPrivateKey(
		o.emulator,
		arkade.ArkadeScriptHash(packet[0].Script),
	)
	in := &ptx.Inputs[0]
	leaf := txscript.NewBaseTapLeaf(in.TaprootLeafScript[0].Script)
	fetcher := txscript.NewCannedPrevOutputFetcher(in.WitnessUtxo.PkScript, in.WitnessUtxo.Value)
	sighash, err := txscript.CalcTapscriptSignaturehash(
		txscript.NewTxSigHashes(ptx.UnsignedTx, fetcher), txscript.SigHashDefault, ptx.UnsignedTx,
		0, fetcher, leaf,
	)
	require.NoError(t, err)
	sig, err := schnorr.Sign(key, sighash)
	require.NoError(t, err)
	hash := leaf.TapHash()
	in.TaprootScriptSpendSig = append(in.TaprootScriptSpendSig, &psbt.TaprootScriptSpendSig{
		XOnlyPubKey: schnorr.SerializePubKey(key.PubKey()), LeafHash: hash[:],
		Signature: sig.Serialize(), SigHash: txscript.SigHashDefault,
	})
}

func TestBuildOnchainSetsThePrevoutAndPaysTheEstimatedFee(t *testing.T) {
	o := newOnchain(t)
	var vsize int
	ptx, err := o.build(t, template.Fee{Dust: 330, Onchain: func(n int) uint64 {
		vsize = n
		return uint64(n) * 3
	}})
	require.NoError(t, err)
	require.NotZero(t, vsize)
	require.Equal(t, int64(onchainAmount-3*vsize), ptx.UnsignedTx.TxOut[0].Value)

	prevouts, err := txutils.GetArkPsbtFields(ptx, 0, arkade.PrevoutTxField)
	require.NoError(t, err)
	require.Len(t, prevouts, 1)
	require.Equal(t, o.source.TxHash(), prevouts[0].TxHash())

	_, err = o.build(t, template.Fee{Dust: 330})
	require.ErrorIs(t, err, template.ErrIneligible, "no fee estimate")
}

func TestBuildOnchainRefusesCoinNotMatchingItsSource(t *testing.T) {
	o := newOnchain(t)
	o.txs = nil
	_, err := o.build(t, template.Fee{Onchain: func(int) uint64 { return 1 }})
	require.ErrorContains(t, err, "missing source output")
}

func TestFinalizeOnchainYieldsAWitnessThatVerifies(t *testing.T) {
	o := newOnchain(t)
	ptx, err := o.build(t, template.Fee{Dust: 330, Onchain: func(int) uint64 { return 500 }})
	require.NoError(t, err)

	unsigned, err := psbt.NewFromRawBytes(bytes.NewReader(mustSerialize(t, ptx)), false)
	require.NoError(t, err)
	_, err = FinalizeOnchain(unsigned)
	require.ErrorIs(t, err, ErrBadTx, "the emulator has not signed")

	o.sign(t, ptx)
	tx, err := FinalizeOnchain(ptx)
	require.NoError(t, err)
	require.NotEmpty(t, tx.TxIn[0].Witness)

	fetcher := txscript.NewCannedPrevOutputFetcher(o.source.TxOut[0].PkScript, onchainAmount)
	engine, err := txscript.NewEngine(
		o.source.TxOut[0].PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		txscript.NewTxSigHashes(tx, fetcher), onchainAmount, fetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}

func TestFinalizeOnchainExitWitnessVerifiesAfterTheDelay(t *testing.T) {
	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{9}, 32))
	owner := schnorr.SerializePubKey(key.PubKey())
	o := newOnchainFrom(t, "onchain_exit.json", map[string][]byte{
		"owner_program": owner, "exit_delay": {3}, "max_fee": {120},
	})
	ptx, err := o.build(t, template.Fee{Dust: 330, Onchain: func(int) uint64 { return 100 }})
	require.NoError(t, err)
	require.EqualValues(t, 3, ptx.UnsignedTx.TxIn[0].Sequence, "the CSV leaf sets nSequence")
	require.Equal(
		t,
		append([]byte{txscript.OP_1, 0x20}, owner...),
		ptx.UnsignedTx.TxOut[0].PkScript,
	)

	o.sign(t, ptx)
	tx, err := FinalizeOnchain(ptx)
	require.NoError(t, err)

	fetcher := txscript.NewCannedPrevOutputFetcher(o.source.TxOut[0].PkScript, onchainAmount)
	engine, err := txscript.NewEngine(
		o.source.TxOut[0].PkScript, tx, 0, txscript.StandardVerifyFlags, nil,
		txscript.NewTxSigHashes(tx, fetcher), onchainAmount, fetcher,
	)
	require.NoError(t, err)
	require.NoError(t, engine.Execute())
}

func mustSerialize(t *testing.T, p *psbt.Packet) []byte {
	t.Helper()
	var b bytes.Buffer
	require.NoError(t, p.Serialize(&b))
	return b.Bytes()
}
