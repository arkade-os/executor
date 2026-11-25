package v1

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/arkade-os/executor/pkg/template/internal/model"

	"github.com/arkade-os/arkd/pkg/ark-lib/txutils"
	"github.com/arkade-os/emulator/pkg/arkade"
	"github.com/arkade-os/executor/pkg/template/artifact"
	"github.com/arkade-os/executor/pkg/template/internal/asm"
	"github.com/arkade-os/executor/pkg/template/internal/failure"
	"github.com/btcsuite/btcd/psbt/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

type spending struct {
	entry   *arkade.EmulatorEntry
	witness wire.TxWitness
	leaf    *artifact.Leaf
	proof   *psbt.TaprootTapLeafScript
}

// Build builds the unsigned transaction spending sources, one per input in slot order.
func (i *Instance) Build(sources []*model.Source, fee model.Fee) (*model.Action, error) {
	return i.BuildCoins(sources, nil, fee)
}

// BuildCoins builds over coins[k] sources per input k, in slot order; nil keeps the instance's
// layout. Only a watch, whose contracts are the same for every coin, takes a new layout.
func (i *Instance) BuildCoins(
	sources []*model.Source,
	coins []int,
	fee model.Fee,
) (*model.Action, error) {
	if coins != nil {
		var err error
		if i, err = i.relayout(coins); err != nil {
			return nil, err
		}
	}
	t := i.tmpl
	if err := checkSources(sources, len(t.inputs)); err != nil {
		return nil, err
	}
	for slot, in := range t.inputs {
		if err := checkSourceScript(in, sources[slot], i.contracts[slot]); err != nil {
			return nil, err
		}
	}
	d := newDraft(t.typ, sources)
	i.lock(d.tx, d.index(0))

	spends, err := i.spends(d)
	if err != nil {
		return nil, err
	}
	scripts, err := i.outputScripts(d, sources)
	if err != nil {
		return nil, err
	}

	charged := uint64(0)
	if t.typ == model.Proof {
		if charged, err = i.intentFee(d, sources, scripts, fee); err != nil {
			return nil, err
		}
	}
	a, err := i.allocate(d, sources, charged, fee.Dust)
	if err != nil {
		return nil, err
	}

	tx := wire.NewMsgTx(d.tx.Version)
	for _, s := range sources {
		tx.AddTxIn(&wire.TxIn{PreviousOutPoint: s.Outpoint, Sequence: wire.MaxTxInSequenceNum})
	}
	i.lock(tx, 0)

	act := &model.Action{Extension: -1, Fee: charged}
	ext, err := i.extension(d, sources, a, spends)
	if err != nil {
		return nil, err
	}
	var extOut *wire.TxOut
	if len(ext) > 0 {
		if extOut, err = ext.TxOut(); err != nil {
			return nil, fmt.Errorf("%w: extension: %v", failure.ErrIneligible, err)
		}
		act.Extension = i.extensionIndex()
	} else if t.packets != nil && t.packets.index >= 0 {
		return nil, fmt.Errorf("%w: nothing to write in the extension", failure.ErrIneligible)
	}
	pos := i.positions(act.Extension)
	for k, o := range t.outputs {
		if t.typ == model.Proof && o.onchain {
			act.Onchain = append(act.Onchain, pos[k])
		}
	}
	slices.Sort(act.Onchain)
	tx.TxOut = make([]*wire.TxOut, len(t.outputs))
	for k := range t.outputs {
		tx.TxOut[k] = &wire.TxOut{Value: int64(a.sats[k]), PkScript: scripts[k]}
	}
	if extOut != nil {
		tx.TxOut = append(tx.TxOut, extOut)
	}
	tx.TxOut = place(tx.TxOut, pos, act.Extension)

	if t.typ == model.Onchain {
		if fee.Onchain == nil {
			return nil, fmt.Errorf("%w: no on-chain fee estimate", failure.ErrIneligible)
		}
		act.Fee = fee.Onchain(vsize(tx, spends))
		if a, err = i.allocate(d, sources, act.Fee, fee.Dust); err != nil {
			return nil, err
		}
		for k := range t.outputs {
			tx.TxOut[pos[k]].Value = int64(a.sats[k])
		}
	}
	if act.Tx, err = i.packet(tx, sources, spends); err != nil {
		return nil, err
	}
	return act, nil
}

// lock sets the resolved nLockTime and nSequences; the template's inputs start at first in tx.
func (i *Instance) lock(tx *wire.MsgTx, first int) {
	tx.LockTime = i.locktime
	for slot, seq := range i.sequences {
		tx.TxIn[first+slot].Sequence = seq
	}
}

// extensionIndex is the stated output_index, else after the offchain outputs of a proof, else
// last.
func (i *Instance) extensionIndex() int {
	t := i.tmpl
	if t.packets.index >= 0 {
		return t.packets.index
	}
	n := 0
	for _, o := range t.outputs {
		if t.typ != model.Proof || !o.onchain {
			n++
		}
	}
	return n
}

// positions maps each output to its place in the transaction; an automatic extension shifts
// what follows it.
func (i *Instance) positions(ext int) []int {
	auto := i.tmpl.packets != nil && i.tmpl.packets.index < 0 && ext >= 0
	out := make([]int, len(i.tmpl.outputs))
	for k, o := range i.tmpl.outputs {
		out[k] = int(o.index)
		if auto && out[k] >= ext {
			out[k]++
		}
	}
	return out
}

func place(outs []*wire.TxOut, pos []int, ext int) []*wire.TxOut {
	placed := make([]*wire.TxOut, len(outs))
	for k, p := range pos {
		placed[p] = outs[k]
	}
	if ext >= 0 {
		placed[ext] = outs[len(pos)]
	}
	return placed
}

func (i *Instance) spends(d *draft) ([]spending, error) {
	out := make([]spending, len(i.tmpl.inputs))
	for slot, in := range i.tmpl.inputs {
		out[slot].leaf = in.selectedLeaf
		out[slot].proof = i.contracts[slot].Proofs[[2]string{in.function, in.leaf}]
		for _, w := range out[slot].leaf.Witness {
			if w.Type == "signature" {
				continue
			}
			raw, err := i.decoded(in.witness[w.Name], w.Type, d, slot)
			if err != nil {
				return nil, fmt.Errorf("input %q witness %q: %w", in.name, w.Name, err)
			}
			out[slot].witness = append(out[slot].witness, raw)
		}
		// metadata lists stack items top first; a witness lists them bottom first
		slices.Reverse(out[slot].witness)
		cov, ok := i.contracts[slot].Covenants[in.function]
		if !ok {
			continue
		}
		e := &arkade.EmulatorEntry{Vin: uint16(slot), Script: bytes.Clone(cov)}
		for _, a := range in.arguments {
			raw, err := i.decoded(a.value, a.field.Type, d, slot)
			if err != nil {
				return nil, fmt.Errorf("input %q argument %q: %w", in.name, a.field.Name, err)
			}
			e.Witness = append(e.Witness, raw)
		}
		// the first flattened input ends on top of the stack
		slices.Reverse(e.Witness)
		out[slot].entry = e
	}
	return out, nil
}

// decoded evaluates b with slot as context input and requires the canonical bytes of typ.
func (i *Instance) decoded(b byteTemplate, typ string, d *draft, slot int) ([]byte, error) {
	raw, err := i.resolve(b, d, d.index(slot))
	if err != nil {
		return nil, err
	}
	if err := asm.CanonicalBytes(typ, raw); err != nil {
		return nil, fmt.Errorf("%w: %v", failure.ErrIneligible, err)
	}
	return raw, nil
}

func (i *Instance) packet(
	tx *wire.MsgTx, sources []*model.Source, spends []spending,
) (*psbt.Packet, error) {
	ptx, err := psbt.NewFromUnsignedTx(tx)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", failure.ErrIneligible, err)
	}
	for slot, s := range sources {
		c := i.contracts[slot]
		proof := *spends[slot].proof
		proof.ControlBlock, proof.Script = bytes.Clone(
			proof.ControlBlock,
		), bytes.Clone(
			proof.Script,
		)
		ptx.Inputs[slot].WitnessUtxo = &wire.TxOut{
			Value: int64(s.Amount), PkScript: bytes.Clone(pkScript(s)),
		}
		ptx.Inputs[slot].TaprootLeafScript = []*psbt.TaprootTapLeafScript{&proof}
		if err := txutils.SetArkPsbtField(
			ptx, slot, txutils.VtxoTaprootTreeField, txutils.TapTree(slices.Clone(c.Tapscripts)),
		); err != nil {
			return nil, err
		}
		if len(spends[slot].witness) > 0 {
			if err := txutils.SetArkPsbtField(
				ptx, slot, txutils.ConditionWitnessField, spends[slot].witness,
			); err != nil {
				return nil, err
			}
		}
	}
	return ptx, nil
}
