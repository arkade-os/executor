package daemon

import (
	"bytes"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestOnchainCoinIsSignedBroadcastThenSucceedsOnceSpent(t *testing.T) {
	d := Start(t)
	templateID, w := onchainDelegation(t, d)

	outpoint := d.Chain.Fund(w.Inputs[0].Script, 10_000)
	d.Chain.Mine(1)
	attempts := waitAttempts(t, d, w.ID, 1, "succeeded")
	require.Equal(t, "onchain", attempts[0].Kind)
	require.Equal(t, []string{outpoint}, attempts[0].Coins)
	require.Empty(t, attempts[0].Error)
	require.Equal(t, 1, d.Emu.Signed)

	txs := d.Chain.Broadcasts()
	require.Len(t, txs, 1)
	require.Equal(t, txs[0].TxHash().String(), attempts[0].Txid)
	require.Equal(t, outpoint, txs[0].TxIn[0].PreviousOutPoint.String())
	require.NotEmpty(t, txs[0].TxIn[0].Witness, "finalized")
	require.Equal(t, "spent", coinsOf(t, d, w.ID)[0].Status)
	require.Zero(t, failures(t, d, templateID))
}

func TestEmulatorRefusingAnOnchainTxChargesTheTemplate(t *testing.T) {
	d := Start(t, WithRetryDelay(time.Hour))
	d.Emu.Refuse(status.Error(codes.InvalidArgument, "covenant not satisfied"))
	templateID, w := onchainDelegation(t, d)

	d.Chain.Fund(w.Inputs[0].Script, 10_000)
	d.Chain.Mine(1)
	attempts := waitAttempts(t, d, w.ID, 1, "failed")
	require.Contains(t, attempts[0].Error, "covenant not satisfied")
	require.Empty(t, d.Chain.Broadcasts())
	require.Eventually(
		t, func() bool { return failures(t, d, templateID) == 1 }, 5*time.Second,
		50*time.Millisecond,
	)
}

func TestOnchainBroadcastFailureIsNotCharged(t *testing.T) {
	d := Start(t, WithRetryDelay(time.Hour))
	d.Chain.RefuseBroadcast(errors.New("electrum: min relay fee not met (code 1)"))
	templateID, w := onchainDelegation(t, d)

	d.Chain.Fund(w.Inputs[0].Script, 10_000)
	d.Chain.Mine(1)
	attempts := waitAttempts(t, d, w.ID, 1, "failed")
	require.Contains(t, attempts[0].Error, "broadcast")
	require.Equal(t, "present", coinsOf(t, d, w.ID)[0].Status)
	time.Sleep(200 * time.Millisecond)
	require.Zero(t, failures(t, d, templateID))
}

func onchainDelegation(t *testing.T, d *Daemon) (string, delegation) {
	t.Helper()
	doc, err := os.ReadFile("../../internal/core/domain/execution/testdata/onchain_arkade.json")
	require.NoError(t, err)
	templateID := registerDoc(t, d, doc)
	w := registerDelegationWith(t, d, templateID, nil)
	require.True(t, w.Inputs[0].Onchain)
	return templateID, w
}

// the fake emulator signs without running the arkade script: this proves the flow, not the covenant
func TestOnchainExitWaitsForItsConfirmationsThenPaysTheOwner(t *testing.T) {
	d := Start(t)
	doc, err := os.ReadFile("../../internal/core/domain/execution/testdata/onchain_exit.json")
	require.NoError(t, err)
	key, _ := btcec.PrivKeyFromBytes(bytes.Repeat([]byte{9}, 32))
	program := schnorr.SerializePubKey(key.PubKey())
	w := registerDelegationWith(t, d, registerDoc(t, d, doc), map[string]string{
		"owner_program": hex.EncodeToString(program), "exit_delay": ScriptNum(3),
		"max_fee": ScriptNum(1000),
	})
	require.True(t, w.Inputs[0].Onchain)

	outpoint := d.Chain.Fund(w.Inputs[0].Script, 10_000)
	d.Chain.Mine(2)
	require.Never(t, func() bool { return len(listAttempts(t, d, w.ID)) > 0 },
		500*time.Millisecond, 20*time.Millisecond, "attempted before the exit delay")
	require.Empty(t, d.Chain.Broadcasts())

	d.Chain.Mine(1)
	attempts := waitAttempts(t, d, w.ID, 1, "succeeded")
	require.Empty(t, attempts[0].Error)
	txs := d.Chain.Broadcasts()
	require.Len(t, txs, 1)
	require.Equal(t, outpoint, txs[0].TxIn[0].PreviousOutPoint.String())
	require.EqualValues(t, 3, txs[0].TxIn[0].Sequence, "satisfies the CSV")
	require.Equal(t, append([]byte{0x51, 0x20}, program...), txs[0].TxOut[0].PkScript)
	require.NotEmpty(t, txs[0].TxIn[0].Witness)
}
