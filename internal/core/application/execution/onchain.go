package execution

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"math"

	delegationapp "github.com/arkade-os/executor/internal/core/application/delegation"
	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/internal/core/domain/execution"
	"github.com/arkade-os/executor/pkg/template"
	"github.com/btcsuite/btcd/psbt/v2"
	log "github.com/sirupsen/logrus"
)

// spendOnchain spends coins with a bitcoin transaction the emulator signs and the chain server
// takes; a is recorded before the emulator is asked.
func (s *Service) spendOnchain(
	ctx context.Context, inst *delegationapp.Instance, a delegation.Attempt,
	coins []delegation.Coin,
) {
	fail := func(err error) { s.send(attemptFailed{attempt: a, err: err, retryIn: s.limits.RetryDelay}) }
	ptx, err := s.buildOnchain(ctx, inst, coins)
	if err != nil {
		fail(err)
		return
	}
	var raw bytes.Buffer
	if err := ptx.Serialize(&raw); err != nil {
		fail(err)
		return
	}
	txid := ptx.UnsignedTx.TxHash().String()
	if err := a.Submit(
		delegation.Tx{Raw: raw.Bytes(), Txid: txid, Onchain: true, Spends: true},
	); err != nil {
		fail(err)
		return
	}
	if err := s.attempts.AddOrUpdate(ctx, &a); err != nil {
		log.WithError(err).WithField("delegation", a.DelegationID).Error("record attempt")
		s.send(skipped{coins: a.Coins})
		return
	}
	s.send(attemptUpdate{attempt: a})
	encoded := base64.StdEncoding.EncodeToString(raw.Bytes())
	signed, err := s.emulator.SubmitOnchainTx(ctx, encoded)
	if err != nil {
		fail(fmt.Errorf("emulator: %w", refusal(ctx, err)))
		return
	}
	merged, err := merge(encoded, signed)
	if err != nil {
		fail(fmt.Errorf("emulator: %w", err))
		return
	}
	tx, err := execution.FinalizeOnchain(merged)
	if err != nil {
		fail(err)
		return
	}
	var wireRaw bytes.Buffer
	if err := tx.Serialize(&wireRaw); err != nil {
		fail(err)
		return
	}
	for i := range a.Txs {
		if a.Txs[i].Onchain && a.Txs[i].Spends {
			a.Txs[i].Raw = wireRaw.Bytes()
		}
	}
	if err := s.attempts.AddOrUpdate(ctx, &a); err != nil {
		log.WithError(err).WithField("delegation", a.DelegationID).Error("record signed attempt")
		s.send(skipped{coins: a.Coins})
		return
	}
	if _, err := s.chain.Broadcast(ctx, tx); err != nil {
		fail(fmt.Errorf("broadcast: %w", err))
		return
	}
	log.WithFields(log.Fields{"delegation": a.DelegationID, "txid": txid}).
		Info("onchain tx broadcast")
	s.send(attemptDone{attempt: a})
}

func (s *Service) buildOnchain(
	ctx context.Context, inst *delegationapp.Instance, coins []delegation.Coin,
) (*psbt.Packet, error) {
	txs, err := s.sources(ctx, coins)
	if err != nil {
		return nil, err
	}
	rate, err := s.chain.FeeRate(ctx)
	if err != nil {
		return nil, fmt.Errorf("fee rate: %w", err)
	}
	fee := template.Fee{
		Dust:    s.info.Dust,
		Onchain: func(vsize int) uint64 { return uint64(math.Ceil(rate * float64(vsize))) },
	}
	return execution.BuildOnchain(inst.Template, inst.Delegation.Inputs, coins, txs, fee)
}
