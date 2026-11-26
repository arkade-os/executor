package v1

import (
	"encoding/hex"
	"testing"

	"github.com/arkade-os/arkd/pkg/ark-lib/extension"
	"github.com/arkade-os/executor/pkg/template/internal/asm"
	"github.com/arkade-os/executor/pkg/template/internal/contract"
	"github.com/arkade-os/executor/pkg/template/internal/failure"
	"github.com/arkade-os/executor/pkg/template/internal/model"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

// poolDoc: funds holds every output of a script; keep repeats per coin with 1,000 of it, pooled
// takes what the coins have left together, less the 25,000 payment.
func poolDoc(t *testing.T) []byte {
	return edited(t, mixedPaymentDoc(t, false), func(m map[string]any) {
		delete(m, "packets")
		m["variables"] = map[string]any{"note": "bytes"}
		m["outputs"] = []any{
			map[string]any{
				"name":    "keep",
				"each":    "funds",
				"value":   map[string]any{"from": "funds", "amount": 1000},
				"locking": map[string]any{"from": "funds"},
				"assets": []any{
					map[string]any{
						"from":   "funds",
						"asset":  hex.EncodeToString(assetID(1)),
						"amount": 1,
					},
				},
			},
			map[string]any{
				"name":  "pooled",
				"value": map[string]any{"from": "funds"},
				"locking": map[string]any{
					"from": "funds",
				},
				"assets": []any{map[string]any{"from": "funds"}},
			},
			map[string]any{
				"name":    "payment",
				"type":    "onchain",
				"value":   map[string]any{"from": "funds", "amount": 25000},
				"locking": payTo,
			},
		}
		m["delegation"] = map[string]any{"trigger": map[string]any{"funds": map[string]any{
			"packet":                7,
			"decode":                "tlv",
			"require":               map[string]any{"tlv:0": "<OPERATOR_KEY>"},
			"capture":               map[string]any{"note": "tlv:1"},
			"expires_after_seconds": 60,
			"all_outputs":           true,
		}}}
	})
}

func TestEachCoin(t *testing.T) {
	tmpl := parseDoc(t, poolDoc(t))
	c, err := contract.Build(
		tmpl.inputs[0].contract.def,
		map[string]asm.Value{
			"owner": {Type: "pubkey", Raw: testKey(t, 2).SerializeCompressed()},
		},
		testKeys(t),
		nil,
	)
	require.NoError(t, err)
	note := map[string][]byte{"note": {1}}
	var src []*model.Source
	for _, n := range []uint64{10_000, 20_000, 30_000} {
		s := coin(t, n, model.Asset{ID: assetID(1), Amount: 3})
		lockTo(s, c.PkScript)
		src = append(src, s)
	}
	inst, err := tmpl.Instantiate(
		model.Context{Keys: testKeys(t), Variables: note, Sources: src, Coins: []int{3}},
	)
	require.NoError(t, err)
	act, err := inst.Build(src, model.Fee{Dust: 330})
	require.NoError(t, err)
	tx := act.Tx.UnsignedTx
	require.Len(t, tx.TxIn, 3)
	require.Len(t, tx.TxOut, 6)
	for k := range 3 {
		require.EqualValues(t, 1000, tx.TxOut[k].Value)
		require.Equal(t, c.PkScript, tx.TxOut[k].PkScript)
	}
	require.EqualValues(t, 60_000-3000-25_000, tx.TxOut[3].Value)
	require.Equal(t, 4, act.Extension)
	require.True(t, extension.IsExtension(tx.TxOut[4].PkScript))
	require.EqualValues(t, 25_000, tx.TxOut[5].Value)

	watch, err := tmpl.Instantiate(model.Context{Keys: testKeys(t), Variables: note})
	require.NoError(t, err)
	again, err := watch.BuildCoins(src, []int{3}, model.Fee{Dust: 330})
	require.NoError(t, err)
	require.Equal(t, tx.TxOut, again.Tx.UnsignedTx.TxOut, "a watch takes the coins as built")
	require.True(t, tmpl.Inputs()[0].AllOutputs)

	_, err = tmpl.Instantiate(model.Context{Keys: testKeys(t), Variables: note, Sources: src})
	require.ErrorIs(t, err, failure.ErrIneligible, "three sources for one coin")

	a, err := inst.allocate(newDraft(inst.tmpl.typ, src), src, 0, 330)
	require.NoError(t, err)
	a1 := hex.EncodeToString(assetID(1))
	require.Equal(t, map[string]uint64{a1: 1}, a.assets[0])
	require.Equal(t, map[string]uint64{a1: 6}, a.assets[3])

	indexed := edited(t, poolDoc(t), func(m map[string]any) {
		for k, o := range m["outputs"].([]any) {
			o.(map[string]any)["index"] = k
		}
	})
	_, err = Parse(indexed, nil)
	require.ErrorIs(t, err, failure.ErrInvalidTemplate, "no index with all_outputs")

	stray := edited(t, poolDoc(t), func(m map[string]any) {
		delete(m, "delegation")
	})
	single := parseDoc(t, stray)
	_, err = single.Instantiate(
		model.Context{Keys: testKeys(t), Variables: note, Sources: src, Coins: []int{3}},
	)
	require.ErrorIs(t, err, failure.ErrIneligible, "several coins need all_outputs")
}

func TestAllOutputsMatch(t *testing.T) {
	keys := model.TriggerKeys{Keys: testKeys(t)}
	tr := parseDoc(t, poolDoc(t)).Triggers()[0]
	ptx, _ := streamed7(t, tlv(0, keys.Operator.SerializeCompressed(), 1, []byte{1}))
	tx := ptx.UnsignedTx
	tx.TxOut = append(tx.TxOut[:2:2], wire.NewTxOut(1000, tx.TxOut[0].PkScript), tx.TxOut[2])
	ptx.Outputs = append(ptx.Outputs[:2:2], psbt.POutput{}, ptx.Outputs[2])
	got, err := tr.Match(ptx, keys, nil, nil)
	require.NoError(t, err)
	require.Len(t, got, 3, "two scripts and the zero-value extension")
	require.Equal(t, []int{0, 2}, got[0].Outputs)
	require.Equal(t, []int{1}, got[1].Outputs)
}
