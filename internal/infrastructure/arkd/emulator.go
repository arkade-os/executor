package arkd

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/arkade-os/arkd/pkg/ark-lib/script"
	"github.com/arkade-os/arkd/pkg/ark-lib/tree"
	"github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	"github.com/arkade-os/emulator/pkg/arkade"
	"github.com/arkade-os/executor/internal/core/domain/execution"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/psbt/v2"
)

// Emulator is arkd emulating the arkade scripts: its signer runs the script of every transaction
// arkd is asked to sign and signs with its key tweaked by that script.
type Emulator struct {
	ark ports.Signer
}

func NewEmulator(ark ports.Signer) *Emulator {
	return &Emulator{ark: ark}
}

func (e *Emulator) Info(ctx context.Context) (ports.EmulatorInfo, error) {
	info, err := e.ark.Info(ctx)
	if err != nil {
		return ports.EmulatorInfo{}, err
	}
	return ports.EmulatorInfo{SignerPubkey: info.SignerPubkey}, nil
}

// SubmitTx submits and finalizes: arkd's signatures are the only ones the checkpoints need, and
// nothing is finalized unless they are valid over the transactions submitted.
func (e *Emulator) SubmitTx(
	ctx context.Context, tx string, checkpoints []string,
) (string, []string, error) {
	txid, signedTx, signedCheckpoints, err := e.ark.SubmitTx(ctx, tx, checkpoints)
	if err != nil {
		return "", nil, err
	}
	if err := verifySigned(tx, checkpoints, signedTx, signedCheckpoints); err != nil {
		return "", nil, fmt.Errorf("arkd signatures: %w", err)
	}
	if err := e.ark.FinalizeTx(ctx, txid, signedCheckpoints); err != nil {
		return "", nil, err
	}
	return signedTx, signedCheckpoints, nil
}

func (e *Emulator) SubmitOnchainTx(ctx context.Context, tx string) (string, bool, error) {
	tx, err := withoutPrevoutTxs(tx)
	if err != nil {
		return "", false, err
	}
	if _, err := e.ark.CosignOnchainTx(ctx, tx); err != nil {
		return "", false, err
	}
	return "", true, nil
}

// SubmitIntent leaves the proof as is: arkd signs it when the intent is registered.
func (e *Emulator) SubmitIntent(_ context.Context, in execution.Intent) (string, error) {
	return in.Proof, nil
}

// SubmitFinalization leaves the forfeits and the commitment as they are: arkd signs them when
// they are submitted.
func (e *Emulator) SubmitFinalization(
	_ context.Context, _ execution.Intent, forfeits []string, _ tree.FlatTxTree, _ string,
) ([]string, string, error) {
	return forfeits, "", nil
}

// arkd attaches the transaction funding each input itself, and its signer refuses a psbt holding
// one twice.
func withoutPrevoutTxs(tx string) (string, error) {
	ptx, err := psbt.NewFromRawBytes(strings.NewReader(tx), true)
	if err != nil {
		return "", fmt.Errorf("onchain tx: %w", err)
	}
	for i := range ptx.Inputs {
		ptx.Inputs[i].Unknowns = slices.DeleteFunc(
			ptx.Inputs[i].Unknowns, func(u *psbt.Unknown) bool {
				prevoutTx, _ := arkade.PrevoutTxField.Decode(u)
				return prevoutTx != nil
			},
		)
	}
	return ptx.B64Encode()
}

// verifySigned checks the ark transaction and every checkpoint came back as submitted, with a
// valid signature of every key their leaves name.
func verifySigned(
	tx string, checkpoints []string, signedTx string, signedCheckpoints []string,
) error {
	signed := map[string]*psbt.Packet{}
	for _, raw := range append([]string{signedTx}, signedCheckpoints...) {
		ptx, err := psbt.NewFromRawBytes(strings.NewReader(raw), true)
		if err != nil {
			return err
		}
		signed[ptx.UnsignedTx.TxID()] = ptx
	}
	for _, raw := range append([]string{tx}, checkpoints...) {
		original, err := psbt.NewFromRawBytes(strings.NewReader(raw), true)
		if err != nil {
			return err
		}
		txid := original.UnsignedTx.TxID()
		if signed[txid] == nil || len(signed[txid].Inputs) != len(original.Inputs) {
			return fmt.Errorf("tx %s is not among the signed ones", txid)
		}
		for i := range original.Inputs {
			original.Inputs[i].TaprootScriptSpendSig = signed[txid].Inputs[i].TaprootScriptSpendSig
		}
		fetcher, err := txutils.GetPrevOutputFetcher(original)
		if err != nil {
			return err
		}
		verified, err := script.VerifyTapscriptSigs(original, fetcher)
		if err != nil {
			return fmt.Errorf("tx %s: %w", txid, err)
		}
		if len(verified) != len(original.Inputs) {
			return fmt.Errorf(
				"tx %s: %d of %d inputs signed", txid, len(verified), len(original.Inputs),
			)
		}
	}
	return nil
}
