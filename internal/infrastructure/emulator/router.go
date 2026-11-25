package emulator

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/arkade-os/arkd/pkg/ark-lib/tree"
	"github.com/arkade-os/emulator/pkg/arkade"
	"github.com/arkade-os/executor/internal/core/domain/execution"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/psbt/v2"
)

// Router sends a transaction to the standalone emulator when a leaf it spends names that
// emulator's key tweaked by the arkade script, to arkd otherwise.
type Router struct {
	standalone ports.Emulator
	key        *btcec.PublicKey
	arkd       ports.Emulator
}

// NewRouter routes between standalone, which signs with key, and arkd.
func NewRouter(standalone ports.Emulator, key *btcec.PublicKey, arkd ports.Emulator) *Router {
	return &Router{standalone: standalone, key: key, arkd: arkd}
}

// Info is the standalone emulator's: arkd's key is its signer key.
func (r *Router) Info(ctx context.Context) (ports.EmulatorInfo, error) {
	return r.standalone.Info(ctx)
}

func (r *Router) SubmitTx(
	ctx context.Context, tx string, checkpoints []string,
) (string, []string, error) {
	emulator, err := r.route(tx)
	if err != nil {
		return "", nil, err
	}
	return emulator.SubmitTx(ctx, tx, checkpoints)
}

func (r *Router) SubmitOnchainTx(ctx context.Context, tx string) (string, bool, error) {
	emulator, err := r.route(tx)
	if err != nil {
		return "", false, err
	}
	return emulator.SubmitOnchainTx(ctx, tx)
}

func (r *Router) SubmitIntent(ctx context.Context, in execution.Intent) (string, error) {
	emulator, err := r.route(in.Proof)
	if err != nil {
		return "", err
	}
	return emulator.SubmitIntent(ctx, in)
}

func (r *Router) SubmitFinalization(
	ctx context.Context, in execution.Intent, forfeits []string, connectorTree tree.FlatTxTree,
	commitmentTx string,
) ([]string, string, error) {
	emulator, err := r.route(in.Proof)
	if err != nil {
		return nil, "", err
	}
	return emulator.SubmitFinalization(ctx, in, forfeits, connectorTree, commitmentTx)
}

func (r *Router) route(tx string) (ports.Emulator, error) {
	ptx, err := psbt.NewFromRawBytes(strings.NewReader(tx), true)
	if err != nil {
		return nil, fmt.Errorf("route to an emulator: %w", err)
	}
	packet, err := arkade.FindEmulatorPacket(ptx.UnsignedTx)
	if err != nil {
		return nil, fmt.Errorf("route to an emulator: %w", err)
	}
	for _, entry := range packet {
		if int(entry.Vin) >= len(ptx.Inputs) {
			continue
		}
		tweaked := schnorr.SerializePubKey(
			arkade.ComputeArkadeScriptPublicKey(r.key, arkade.ArkadeScriptHash(entry.Script)),
		)
		for _, leaf := range ptx.Inputs[entry.Vin].TaprootLeafScript {
			if bytes.Contains(leaf.Script, tweaked) {
				return r.standalone, nil
			}
		}
	}
	return r.arkd, nil
}
