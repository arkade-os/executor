package v1

import (
	"encoding/hex"
	"testing"

	"github.com/arkade-os/arkd/pkg/ark-lib/asset"
	"github.com/arkade-os/executor/pkg/template/internal/failure"
	"github.com/arkade-os/executor/pkg/template/internal/model"
	"github.com/stretchr/testify/require"
)

func TestIssuesAndBurns(t *testing.T) {
	a1, a2 := hex.EncodeToString(assetID(1)), hex.EncodeToString(assetID(2))
	doc := edited(t, mixedPaymentDoc(t, true), func(m map[string]any) {
		m["issues"] = []any{
			map[string]any{"name": "ticket", "control": map[string]any{"issue": "ctrl"},
				"metadata": map[string]any{"name": "5469636b6574"}},
			map[string]any{"name": "ctrl"},
			map[string]any{"name": "supply", "asset": a2},
		}
		m["outputs"].([]any)[0].(map[string]any)["assets"] = []any{
			map[string]any{"from": "funds"},
			map[string]any{"issue": "ticket", "amount": 5},
			map[string]any{"issue": "ctrl", "amount": 1},
			map[string]any{"issue": "supply", "amount": 4},
		}
		m["burns"] = []any{map[string]any{"from": "funds", "asset": a1, "amount": 3}}
	})
	inst, d, src := instanceOf(t, doc, coin(t, 100_000, model.Asset{ID: assetID(1), Amount: 10}))
	a, err := inst.allocate(d, src, 0, 330)
	require.NoError(t, err)
	require.Equal(t, map[string]uint64{a1: 7, a2: 4}, a.assets[0])
	require.Equal(t, map[string]uint64{a1: 3}, a.assets[2], "the burn")

	p, err := assetPacket(inst, d, src, a)
	require.NoError(t, err)
	groups := p.(asset.Packet)
	require.Len(t, groups, 4)
	require.Nil(t, groups[0].AssetId)
	require.EqualValues(t, 1, groups[0].ControlAsset.GroupIndex)
	require.EqualValues(t, 5, groups[0].Outputs[0].Amount)
	require.Equal(t, "name", string(groups[0].Metadata[0].Key))
	require.Nil(t, groups[1].AssetId)
	require.Equal(t, a1, hex.EncodeToString(mustSerialize(t, groups[2].AssetId)))
	require.EqualValues(t, 10, groups[2].Inputs[0].Amount)
	require.EqualValues(t, 7, groups[2].Outputs[0].Amount)
	require.Empty(t, groups[3].Inputs)
	require.EqualValues(t, 4, groups[3].Outputs[0].Amount)

	for name, edit := range map[string]func(map[string]any){
		"unrouted issue": func(m map[string]any) {
			m["issues"] = append(m["issues"].([]any), map[string]any{"name": "spare"})
		},
		"control names itself": func(m map[string]any) {
			m["issues"].([]any)[1].(map[string]any)["control"] = map[string]any{"issue": "ctrl"}
		},
		"two catch-alls": func(m map[string]any) {
			m["burns"] = []any{map[string]any{"from": "funds"}}
		},
	} {
		_, err := Parse(edited(t, doc, edit), nil)
		require.ErrorIs(t, err, failure.ErrInvalidTemplate, name)
	}
}

func mustSerialize(t *testing.T, id *asset.AssetId) []byte {
	t.Helper()
	b, err := id.Serialize()
	require.NoError(t, err)
	return b
}
