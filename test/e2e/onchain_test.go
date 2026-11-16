//go:build e2e

package e2e

import (
	"encoding/hex"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/arkade-os/executor/test/daemon"
	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/stretchr/testify/require"
)

func TestOnchainDelegationIsSignedAndBroadcast(t *testing.T) {
	d := start(t)
	doc, err := os.ReadFile("../../internal/core/domain/execution/testdata/onchain_arkade.json")
	require.NoError(t, err)
	w := registerDelegation(t, d, registerDocument(t, d, doc), nil)
	faucet(t, w.Address, 20_000)

	require.Eventually(t, func() bool {
		attempts := attemptsOf(t, d, w.Id)
		return len(attempts) == 1 && attempts[0].Status == "succeeded"
	}, 2*time.Minute, time.Second, "the payout was never broadcast")
}

func TestOnchainExitPaysTheOwnerAfterTheDelay(t *testing.T) {
	d := start(t)
	doc, err := os.ReadFile("../../internal/core/domain/execution/testdata/onchain_exit.json")
	require.NoError(t, err)
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	program := schnorr.SerializePubKey(key.PubKey())
	w := registerDelegation(t, d, registerDocument(t, d, doc), map[string]string{
		"owner_program": hex.EncodeToString(program), "exit_delay": daemon.ScriptNum(3),
		"max_fee": daemon.ScriptNum(1000),
	})
	faucet(t, w.Address, 20_000)

	addr, err := address.NewAddressTaproot(program, &chaincfg.RegressionNetParams)
	require.NoError(t, err)
	received := func() bool {
		var utxos []struct {
			Value int64 `json:"value"`
		}
		getJSON(t, explorerURL+"/address/"+addr.EncodeAddress()+"/utxo", &utxos)
		return len(utxos) == 1 && utxos[0].Value > 19_000
	}
	mine(t, 1) // the faucet mined the first confirmation
	require.Never(t, func() bool { return len(attemptsOf(t, d, w.Id)) > 0 },
		5*time.Second, time.Second, "attempted before the exit delay")
	mine(t, 1)
	require.Eventually(t, func() bool {
		attempts := attemptsOf(t, d, w.Id)
		return len(attempts) == 1 && attempts[0].Status == "succeeded"
	}, 2*time.Minute, time.Second, "the exit was never broadcast")
	mine(t, 1)
	require.Eventually(t, received, 30*time.Second, time.Second, "the owner was not paid")
}

func mine(t *testing.T, n int) {
	t.Helper()
	out, err := exec.Command("nigiri", "rpc", "-generate", strconv.Itoa(n)).CombinedOutput()
	require.NoError(t, err, string(out))
}
