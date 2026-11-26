package v1

import (
	"testing"

	"github.com/arkade-os/arkd/pkg/ark-lib/extension"
	"github.com/arkade-os/executor/pkg/template/internal/failure"
	"github.com/arkade-os/executor/pkg/template/internal/model"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

const payTo = "5120f9308a019258c31049344f85f89d5229b531c845836f99b08601f113bce036f9"

func onchainFee(int) uint64 { return 600 }

func TestUnindexedProofPlacesTheExtension(t *testing.T) {
	doc := edited(t, mixedPaymentDoc(t, false), func(m map[string]any) {
		delete(m, "packets")
		for _, o := range m["outputs"].([]any) {
			delete(o.(map[string]any), "index")
		}
	})
	inst, _, src := instanceOf(t, doc, sourceWith(t, 100_000))
	act, err := inst.Build(src, model.Fee{Dust: 330})
	require.NoError(t, err)
	tx := act.Tx.UnsignedTx
	require.Len(t, tx.TxOut, 3)
	require.EqualValues(t, 75_000, tx.TxOut[0].Value)
	require.Equal(t, 1, act.Extension)
	require.True(t, extension.IsExtension(tx.TxOut[1].PkScript))
	require.EqualValues(t, 25_000, tx.TxOut[2].Value)

	swapped := edited(t, doc, func(m map[string]any) {
		o := m["outputs"].([]any)
		o[0], o[1] = o[1], o[0]
	})
	_, err = Parse(swapped, nil)
	require.ErrorIs(t, err, failure.ErrInvalidTemplate, "a proof lists offchain outputs first")

	stray := edited(
		t,
		doc,
		func(m map[string]any) { m["packets"] = map[string]any{"output_index": 1} },
	)
	_, err = Parse(stray, nil)
	require.ErrorIs(t, err, failure.ErrInvalidTemplate, "output_index goes with indexed outputs")
}

func TestValueDraws(t *testing.T) {
	doc := edited(t, twoInputsOnchain(t), func(m map[string]any) {
		m["outputs"] = []any{
			map[string]any{"name": "merged", "value": []any{
				map[string]any{"from": "a"}, map[string]any{"from": "b", "amount": 10_000},
			}, "locking": payTo},
			map[string]any{"name": "rest", "value": map[string]any{"from": "b"}, "locking": payTo},
		}
	})
	inst, _, src := instanceOf(t, doc, sourceWith(t, 50_000), sourceWith(t, 30_000))
	act, err := inst.Build(src, model.Fee{Dust: 330, Onchain: onchainFee})
	require.NoError(t, err)
	require.EqualValues(t, 50_000-600+10_000, act.Tx.UnsignedTx.TxOut[0].Value)
	require.EqualValues(t, 20_000, act.Tx.UnsignedTx.TxOut[1].Value)
	require.Equal(t, -1, act.Extension)

	twice := edited(t, doc, func(m map[string]any) {
		m["outputs"].([]any)[1].(map[string]any)["value"] = map[string]any{"from": "a"}
	})
	_, err = Parse(twice, nil)
	require.ErrorIs(t, err, failure.ErrInvalidTemplate, "two remainder draws on a")
}

func TestIndexedInputs(t *testing.T) {
	doc := edited(t, twoInputsOnchain(t), func(m map[string]any) {
		in := m["inputs"].([]any)
		in[0].(map[string]any)["index"] = 1
		in[1].(map[string]any)["index"] = 0
	})
	tmpl := parseDoc(t, doc)
	require.Equal(t, "b", tmpl.Inputs()[0].Name)

	dup := edited(
		t,
		doc,
		func(m map[string]any) { m["inputs"].([]any)[0].(map[string]any)["index"] = 0 },
	)
	_, err := Parse(dup, nil)
	require.ErrorIs(t, err, failure.ErrInvalidTemplate)
}

func TestLeafTimelocks(t *testing.T) {
	doc := edited(t, fixture(t, "minimal.json"), func(m map[string]any) {
		releaseFn(m)["asm"] = []any{
			"700", "OP_CHECKLOCKTIMEVERIFY", "OP_DROP", "144", "OP_CHECKSEQUENCEVERIFY", "OP_DROP",
			"<BATCH_COSIGNER_KEY>", "OP_CHECKSIG",
		}
	})
	build := func(doc []byte) (*wire.MsgTx, error) {
		inst, _, src := instanceOf(t, doc, sourceWith(t, 100_000))
		act, err := inst.Build(src, model.Fee{Dust: 330, Onchain: onchainFee})
		if err != nil {
			return nil, err
		}
		return act.Tx.UnsignedTx, nil
	}
	tx, err := build(doc)
	require.NoError(t, err)
	require.EqualValues(t, 700, tx.LockTime)
	require.EqualValues(t, 144, tx.TxIn[0].Sequence)

	tx, err = build(edited(t, doc, func(m map[string]any) {
		m["locktime"] = 800
		m["inputs"].([]any)[0].(map[string]any)["sequence"] = 200
	}))
	require.NoError(t, err)
	require.EqualValues(t, 800, tx.LockTime)
	require.EqualValues(t, 200, tx.TxIn[0].Sequence)

	for name, edit := range map[string]func(map[string]any){
		"locktime below the leaf":  func(m map[string]any) { m["locktime"] = 600 },
		"locktime of another kind": func(m map[string]any) { m["locktime"] = 1_800_000_000 },
		"sequence below the leaf": func(m map[string]any) {
			m["inputs"].([]any)[0].(map[string]any)["sequence"] = 100
		},
	} {
		_, err := parseDoc(t, edited(t, doc, edit)).Instantiate(model.Context{Keys: testKeys(t)})
		require.ErrorIs(t, err, failure.ErrIneligible, name)
	}

	plain, err := build(fixture(t, "minimal.json"))
	require.NoError(t, err)
	require.Zero(t, plain.LockTime)
	require.EqualValues(t, wire.MaxTxInSequenceNum, plain.TxIn[0].Sequence)

	stated, err := build(
		edited(t, fixture(t, "minimal.json"), func(m map[string]any) { m["locktime"] = 5 }),
	)
	require.NoError(t, err)
	require.EqualValues(t, 5, stated.LockTime)
	require.EqualValues(
		t,
		wire.MaxTxInSequenceNum-1,
		stated.TxIn[0].Sequence,
		"the locktime is enforced",
	)

	_, err = Parse(edited(t, []byte(releaseDoc), func(m map[string]any) { m["locktime"] = 5 }), nil)
	require.ErrorIs(t, err, failure.ErrUnsupported, "arkd sets a virtual transaction's locktime")
}

func releaseFn(m map[string]any) map[string]any {
	def := m["inputs"].([]any)[0].(map[string]any)["contract"].(map[string]any)["definition"].(map[string]any)
	return def["functions"].([]any)[0].(map[string]any)["leaves"].([]any)[0].(map[string]any)
}

// twoInputsOnchain is minimal.json with inputs a and b, each paying its own output.
func twoInputsOnchain(t *testing.T) []byte {
	t.Helper()
	return edited(t, fixture(t, "minimal.json"), func(m map[string]any) {
		in := m["inputs"].([]any)[0].(map[string]any)
		a, b := map[string]any{}, map[string]any{}
		for k, v := range in {
			a[k], b[k] = v, v
		}
		a["name"], b["name"] = "a", "b"
		m["inputs"] = []any{a, b}
		m["outputs"] = []any{
			map[string]any{"name": "pa", "value": map[string]any{"from": "a"}, "locking": payTo},
			map[string]any{"name": "pb", "value": map[string]any{"from": "b"}, "locking": payTo},
		}
		m["fees"] = map[string]any{"from": "a", "max": 1000}
	})
}
