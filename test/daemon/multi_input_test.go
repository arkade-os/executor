package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTwoVirtualInputsSpendTogether(t *testing.T) {
	d := Start(t)
	doc, err := os.ReadFile("../../internal/core/domain/execution/testdata/virtual_two.json")
	require.NoError(t, err)
	w := registerDelegationWith(t, d, registerDoc(t, d, doc), nil)
	require.Len(t, w.Inputs, 2)
	require.NotEqual(t, w.Inputs[0].Script, w.Inputs[1].Script)
	require.NotEmpty(t, w.Inputs[0].Address)
	require.NotEmpty(t, w.Inputs[1].Address)
	require.NotEqual(t, w.Inputs[0].Address, w.Inputs[1].Address)

	first := d.Arkd.Fund(w.Inputs[0].Script, 10_000, time.Now().Add(time.Hour))
	require.Never(t, func() bool { return len(listAttempts(t, d, w.ID)) > 0 },
		400*time.Millisecond, 20*time.Millisecond)
	second := d.Arkd.Fund(w.Inputs[1].Script, 20_000, time.Now().Add(time.Hour))
	attempts := waitAttempts(t, d, w.ID, 1, "submitted")
	require.Equal(t, []string{first, second}, attempts[0].Coins)
	require.Equal(t, "virtual", attempts[0].Kind)
	require.Equal(t, []string{attempts[0].Txid}, d.Arkd.submitted())

	d.Arkd.Spend(first, attempts[0].Txid)
	require.Never(t, func() bool {
		return listAttempts(t, d, w.ID)[0].Status == "succeeded"
	}, 400*time.Millisecond, 20*time.Millisecond)
	d.Arkd.Spend(second, attempts[0].Txid)
	require.Eventually(t, func() bool {
		return listAttempts(t, d, w.ID)[0].Status == "succeeded"
	}, 5*time.Second, 50*time.Millisecond)
	coins := coinsOf(t, d, w.ID)
	require.Len(t, coins, 2)
	for _, c := range coins {
		require.Equal(t, "spent", c.Status)
		require.Equal(t, attempts[0].Txid, c.SpentBy)
	}
}

func TestTwoOnchainInputsAreSignedTogether(t *testing.T) {
	d := Start(t)
	doc, err := os.ReadFile("../../internal/core/domain/execution/testdata/onchain_two.json")
	require.NoError(t, err)
	w := registerDelegationWith(t, d, registerDoc(t, d, doc), nil)
	require.Len(t, w.Inputs, 2)
	require.True(t, w.Inputs[0].Onchain)
	require.True(t, w.Inputs[1].Onchain)
	require.NotEqual(t, w.Inputs[0].Script, w.Inputs[1].Script)

	first := d.Chain.Fund(w.Inputs[0].Script, 10_000)
	d.Chain.Mine(1)
	require.Never(t, func() bool { return len(listAttempts(t, d, w.ID)) > 0 },
		400*time.Millisecond, 20*time.Millisecond)
	second := d.Chain.Fund(w.Inputs[1].Script, 20_000)
	d.Chain.Mine(1)
	attempts := waitAttempts(t, d, w.ID, 1, "succeeded")
	require.Equal(t, []string{first, second}, attempts[0].Coins)
	require.Equal(t, "onchain", attempts[0].Kind)
	require.Empty(t, attempts[0].Error)

	txs := d.Chain.Broadcasts()
	require.Len(t, txs, 1)
	require.Equal(t, attempts[0].Txid, txs[0].TxHash().String())
	require.Equal(t, first, txs[0].TxIn[0].PreviousOutPoint.String())
	require.Equal(t, second, txs[0].TxIn[1].PreviousOutPoint.String())
	require.NotEmpty(t, txs[0].TxIn[0].Witness)
	require.NotEmpty(t, txs[0].TxIn[1].Witness)
	require.EqualValues(t, 10_000, txs[0].TxOut[0].Value)
	var paid int64
	for _, out := range txs[0].TxOut {
		paid += out.Value
	}
	fee := int64(30_000) - paid
	require.Positive(t, fee)
	require.Equal(t, int64(20_000)-fee, txs[0].TxOut[1].Value)
}
