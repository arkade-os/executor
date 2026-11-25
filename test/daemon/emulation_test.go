package daemon

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/arkade-os/arkd/pkg/ark-lib/script"
	"github.com/arkade-os/executor/internal/config"
	"github.com/arkade-os/executor/internal/infrastructure/arkd"
	"github.com/arkade-os/executor/templates"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

func TestBothEmulatorsServeOneDaemon(t *testing.T) {
	d := Start(t, WithSessions(time.Second, 128), WithDefaultTemplates)
	require.Equal(
		t, len(templates.Artifacts)+len(templates.EmulatorArtifacts),
		tableRows(t, d, "artifacts"),
	)
	old := renewalDelegation(
		t, d, registerDoc(t, d, templates.EmulatorTemplates["renew.json"]), 1024, 1000,
	)
	signerOnly := renewalDelegation(
		t, d, registerDoc(t, d, templates.Templates["renew.json"]), 1024, 1000,
	)
	require.Len(t, renewKeys(t, old), 2, "arkd's key next to the emulator's tweaked key")
	require.Len(t, renewKeys(t, signerOnly), 1, "arkd's tweaked key alone")
	require.False(t, renewKeys(t, signerOnly)[0].IsEqual(d.Server))

	expiry := time.Now().Add(500 * time.Second)
	d.Arkd.FundAged(signerOnly.Inputs[0].Script, 10_000, time.Hour, expiry)
	waitAttempts(t, d, signerOnly.ID, 1, "succeeded")
	require.Zero(t, d.Emu.Intents, "arkd signs the leaf naming its tweaked key")
	require.Zero(t, d.Emu.Finalizations)

	d.Arkd.FundAged(old.Inputs[0].Script, 10_000, time.Hour, expiry)
	waitAttempts(t, d, old.ID, 1, "succeeded")
	require.Equal(t, 1, d.Emu.Intents, "the emulator signs the leaf naming its tweaked key")
	require.Equal(t, 1, d.Emu.Finalizations)
}

func TestArkdFinalizesTheOffchainSpendOfItsTweakedKey(t *testing.T) {
	d := Start(t, WithEncryptionKey)
	templateID := registerDoc(t, d, signerKeyed(fixture(t, "vhtlc_claim.json")))
	w := registerDelegationWith(t, d, templateID, newSwap(t, d, templateID).variables)

	outpoint := d.Arkd.Fund(w.Inputs[0].Script, 10_000, time.Now().Add(time.Hour))
	var attempts []attempt
	require.Eventually(t, func() bool {
		attempts = listAttempts(t, d, w.ID)
		return len(attempts) == 1 && attempts[0].Status == "submitted" &&
			len(d.Arkd.finalized()) == 1
	}, 5*time.Second, 50*time.Millisecond)
	require.Equal(t, []string{outpoint}, attempts[0].Coins)
	require.Empty(t, attempts[0].Error)
	require.Equal(t, []string{attempts[0].Txid}, d.Arkd.submitted())
	require.Equal(t, d.Arkd.submitted(), d.Arkd.finalized())
	require.Zero(t, d.Emu.Signed)
}

func TestArkdBroadcastsTheOnchainSpendOfItsTweakedKey(t *testing.T) {
	d := Start(t)
	doc, err := os.ReadFile("../../internal/core/domain/execution/testdata/onchain_arkade.json")
	require.NoError(t, err)
	templateID := registerDoc(t, d, signerKeyed(doc))
	w := registerDelegationWith(t, d, templateID, nil)

	outpoint := d.Chain.Fund(w.Inputs[0].Script, 10_000)
	d.Chain.Mine(1)
	attempts := waitAttempts(t, d, w.ID, 1, "succeeded")
	require.Equal(t, []string{outpoint}, attempts[0].Coins)
	require.Empty(t, attempts[0].Error)
	require.Zero(t, d.Emu.Signed)

	txs := d.Chain.Broadcasts()
	require.Len(t, txs, 1, "arkd broadcasts, the daemon does not broadcast again")
	require.Equal(t, txs[0].TxHash().String(), attempts[0].Txid)
	require.Equal(t, outpoint, txs[0].TxIn[0].PreviousOutPoint.String())
	require.Zero(t, failures(t, d, templateID))
}

func TestWithoutAnEmulatorOnlyArkdsTemplatesAreServed(t *testing.T) {
	d := Start(t, WithSessions(time.Second, 128), WithDefaultTemplates, WithoutEmulator)
	require.Equal(t, len(templates.Templates), tableRows(t, d, "templates"))
	code, body := get(t, d.RestURL+"/v1/info")
	require.Equal(t, http.StatusOK, code, string(body))
	var info struct {
		EmulatorPubkey string `json:"emulatorPubkey"`
	}
	require.NoError(t, json.Unmarshal(body, &info))
	require.Empty(t, info.EmulatorPubkey)

	for _, doc := range templates.EmulatorArtifacts {
		code, body := post(t, d.AdminURL+"/v1/artifact", map[string]string{"document": string(doc)})
		require.Equal(t, http.StatusOK, code, string(body))
	}
	code, body = post(t, d.RestURL+"/v1/delegation", map[string]any{
		"templateId": registerDoc(t, d, templates.EmulatorTemplates["renew.json"]),
		"variables":  renewalVariables(t),
	})
	require.Equal(t, http.StatusBadRequest, code, string(body))
	require.Contains(t, string(body), "missing emulator key")

	w := renewalDelegation(
		t, d, registerDoc(t, d, templates.Templates["renew.json"]), 1024, 1000,
	)
	d.Arkd.FundAged(w.Inputs[0].Script, 10_000, time.Hour, time.Now().Add(500*time.Second))
	waitAttempts(t, d, w.ID, 1, "succeeded")
}

func TestStandaloneEmulatorSigningWithTheSignerKeyIsRefused(t *testing.T) {
	dsn := DatabaseURL()
	ResetDatabase(t, dsn)
	standin := newArk()
	cfg := config.Default()
	cfg.DatabaseURL, cfg.Port, cfg.AdminPort = dsn, FreePort(t), FreePort(t)
	cfg.DefaultTemplates, cfg.CosignerKey, cfg.Chain = false, newKey(4), newChain()
	cfg.Ark, cfg.Indexer = standin, arkd.NewIndexer(standin, standin)
	cfg.Emulator = &emulator{signer: standin.server.PubKey(), key: standin.server, ark: standin}

	_, err := config.NewDaemon(t.Context(), cfg)
	require.ErrorContains(t, err, "signs with arkd's signer key")
}

func signerKeyed(doc []byte) []byte {
	return bytes.ReplaceAll(doc, []byte("<EMULATOR_KEY:"), []byte("<SIGNER_KEY:"))
}

func renewKeys(t *testing.T, w delegation) []*btcec.PublicKey {
	t.Helper()
	owner, err := hex.DecodeString(w.Variables["owner"])
	require.NoError(t, err)
	for _, leaf := range w.Inputs[0].Tapscripts {
		raw, err := hex.DecodeString(leaf)
		require.NoError(t, err)
		closure, err := script.DecodeClosure(raw)
		require.NoError(t, err)
		multisig, ok := closure.(*script.MultisigClosure)
		if ok && !bytes.Contains(raw, owner[1:]) {
			return multisig.PubKeys
		}
	}
	require.Fail(t, "no renew leaf")
	return nil
}
