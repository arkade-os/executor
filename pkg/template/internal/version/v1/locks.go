package v1

import (
	"fmt"
	"math"
	"strings"

	"github.com/arkade-os/emulator/pkg/arkade"
	"github.com/arkade-os/executor/pkg/template/internal/asm"
	"github.com/arkade-os/executor/pkg/template/internal/failure"
	"github.com/arkade-os/executor/pkg/template/internal/model"
	"github.com/arkade-os/executor/pkg/template/internal/version/v1/schema"
	"github.com/btcsuite/btcd/txscript/v2"
	"github.com/btcsuite/btcd/wire/v2"
)

const (
	locktimeThreshold = 500_000_000 // nLockTime below is a height, at or above a timestamp
	seqDisabled       = 1 << 31
	seqSeconds        = 1 << 22
	seqMask           = 0xffff
)

// locks reads the stated locktime and sequences; arkd derives both itself for a virtual spend and
// a proof carries no locktime.
func (c *compiler) locks() error {
	spec := func(v *schema.IntOrVar, what string) (*lockSpec, error) {
		if v == nil {
			return nil, nil
		}
		if c.t.typ == model.Virtual || c.t.typ == model.Proof && what == "locktime" {
			return nil, fmt.Errorf("%w: %s in a %s template", failure.ErrUnsupported, what, c.t.typ)
		}
		n, name, err := c.intVariable(*v)
		return &lockSpec{n: n, variable: name}, err
	}
	var err error
	if c.t.locktime, err = spec(c.doc.Locktime, "locktime"); err != nil {
		return err
	}
	for _, s := range c.doc.Inputs {
		slot, _ := c.inputIndex(s.Name, "sequence")
		if c.t.inputs[slot].sequence, err = spec(s.Sequence, "sequence"); err != nil {
			return err
		}
	}
	return nil
}

func (i *Instance) lockValue(l *lockSpec) uint32 {
	if l.variable == "" {
		return uint32(l.n)
	}
	n, _ := asm.ParseScriptNum(i.vars[l.variable]) // range checked with the variables
	return uint32(n)
}

// leafLocks reads <x> OP_CHECKLOCKTIMEVERIFY and <x> OP_CHECKSEQUENCEVERIFY from a leaf script,
// keeping the highest of each.
func leafLocks(script []byte) (cltv, csv *uint32) {
	tok := txscript.MakeScriptTokenizer(0, script)
	var prevOp byte
	var prev []byte
	for tok.Next() {
		op := tok.Opcode()
		if op == txscript.OP_CHECKLOCKTIMEVERIFY || op == txscript.OP_CHECKSEQUENCEVERIFY {
			n, ok := pushedNum(prevOp, prev)
			dst := &cltv
			if op == txscript.OP_CHECKSEQUENCEVERIFY {
				dst = &csv
			}
			if ok && (*dst == nil || n > **dst) {
				*dst = &n
			}
		}
		prevOp, prev = op, tok.Data()
	}
	return cltv, csv
}

func pushedNum(op byte, data []byte) (uint32, bool) {
	if op >= txscript.OP_1 && op <= txscript.OP_16 {
		return uint32(op - txscript.OP_1 + 1), true
	}
	n, err := arkade.MakeScriptNum(data, true, 5)
	if err != nil || n < 0 || n > math.MaxUint32 {
		return 0, false
	}
	return uint32(n), true
}

// resolveLocks sets the transaction's nLockTime and every input's nSequence: a stated value, or
// the one the spent leaf asks for.
func (i *Instance) resolveLocks(csvs []*uint32) error {
	t := i.tmpl
	for _, lt := range i.locktimes {
		if lt != nil {
			i.locktime = max(i.locktime, uint32(*lt))
		}
	}
	if t.locktime != nil {
		n := i.lockValue(t.locktime)
		if n < i.locktime || i.locktime > 0 &&
			(n >= locktimeThreshold) != (i.locktime >= locktimeThreshold) {
			return fmt.Errorf(
				"%w: locktime %d is not accepted by the spent leaves",
				failure.ErrIneligible,
				n,
			)
		}
		i.locktime = n
	}
	for slot, in := range t.inputs {
		seq := uint32(wire.MaxTxInSequenceNum)
		if i.locktime > 0 {
			seq--
		}
		csv := csvs[slot]
		if csv != nil && t.typ != model.Virtual {
			seq = *csv
		}
		if in.sequence != nil {
			seq = i.lockValue(in.sequence)
			if csv != nil && !acceptsCSV(seq, *csv) {
				return fmt.Errorf(
					"%w: input %q: sequence %d is not accepted by its leaf",
					failure.ErrIneligible,
					in.name,
					seq,
				)
			}
		}
		i.sequences = append(i.sequences, seq)
	}
	return nil
}

func (i *Instance) TxLocktime() uint32 { return i.locktime }

func (i *Instance) Sequence(slot int) uint32 {
	slot = i.firstCoin(slot)
	if slot < 0 || slot >= len(i.sequences) {
		return wire.MaxTxInSequenceNum
	}
	return i.sequences[slot]
}

func (i *Instance) OfflineSpend(directOnchain bool) error {
	var arkd, emulator bool
	for _, in := range i.tmpl.inputs {
		leaf := in.selectedLeaf
		if leaf == nil {
			return fmt.Errorf("%w: input %q has no spend leaf", failure.ErrUnsupported, in.name)
		}
		for _, w := range leaf.Witness {
			if w.Type == "signature" && !w.Injected {
				return fmt.Errorf(
					"%w: input %q: leaf %q needs signature %q, which nobody injects",
					failure.ErrUnsupported, in.name, leaf.Name, w.Name,
				)
			}
		}
		signer, emulated, err := offlineSigners(leaf.Asm, directOnchain)
		if err != nil {
			return fmt.Errorf("%w: input %q: %v", failure.ErrUnsupported, in.name, err)
		}
		arkd, emulator = arkd || signer, emulator || emulated
	}
	if arkd && emulator {
		return fmt.Errorf(
			"%w: the spent leaves name both arkd's tweaked key and the emulator's",
			failure.ErrUnsupported,
		)
	}
	return nil
}

// arkd settles a leaf only when it names its signer key, plain or tweaked by an arkade script, and
// its plain key signs no bitcoin transaction.
func offlineSigners(tokens []string, directOnchain bool) (signer, emulator bool, err error) {
	server := false
	for n, tok := range tokens {
		switch tok {
		case "OP_CHECKSIG", "OP_CHECKSIGVERIFY", "OP_CHECKSIGADD":
		default:
			continue
		}
		if n == 0 {
			return false, false, fmt.Errorf("%w: checksig has no key", failure.ErrUnsupported)
		}
		key := tokens[n-1]
		switch {
		case strings.HasPrefix(key, "<EMULATOR_KEY:") && strings.HasSuffix(key, ">"):
			emulator = true
		case strings.HasPrefix(key, "<SIGNER_KEY:") && strings.HasSuffix(key, ">"):
			signer = true
		case key == "<SERVER_KEY>" && !directOnchain:
			server = true
		default:
			return false, false, fmt.Errorf(
				"%w: signer %s is not available offline", failure.ErrUnsupported, key,
			)
		}
	}
	if !directOnchain && !server && !signer {
		return false, false, fmt.Errorf(
			"%w: arkd signs no key of the leaf", failure.ErrUnsupported,
		)
	}
	return signer, emulator, nil
}

// acceptsCSV reports whether nSequence seq satisfies <req> OP_CHECKSEQUENCEVERIFY (BIP 112).
func acceptsCSV(seq, req uint32) bool {
	if req&seqDisabled != 0 {
		return true
	}
	return seq&seqDisabled == 0 && seq&seqSeconds == req&seqSeconds && seq&seqMask >= req&seqMask
}
