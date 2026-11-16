package v1

import (
	"bytes"
	"cmp"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"math"
	"math/bits"
	"slices"

	"github.com/arkade-os/executor/pkg/template/internal/asm"
	"github.com/arkade-os/executor/pkg/template/internal/contract"
	"github.com/arkade-os/executor/pkg/template/internal/version/v1/schema"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/txscript/v2"
)

type locking struct {
	from     int          // input index, -1 when unused
	script   byteTemplate // set when the locking is a script template
	contract *construction
}

type output struct {
	name    string
	index   uint16 // its position; with an automatic extension, before the extension is placed
	onchain bool
	from    int // the input of its first draw, the context of its expressions
	draws   []draw
	locking locking
	assets  []assetRoute
	each    int // the input it repeats for, -1 without each
	coin    int // in an instance, the coin slot of a copy of an each output, else -1
}

type draw struct {
	from   int
	amount byteTemplate // nil: the remainder
}

// 21 million bitcoin in satoshis
const maxAmount = 21e14

type allocation struct {
	sats   []uint64            // per output, template order
	assets []map[string]uint64 // per output then burn: hex asset id → amount
	minted [][]uint64          // per issue, per output: units of a new asset
	fee    uint64
}

func (c *compiler) output(s *schema.Output) error {
	if slices.ContainsFunc(c.t.outputs, func(x output) bool { return x.name == s.Name }) {
		return bad("duplicate output %q", s.Name)
	}
	o := output{name: s.Name, index: uint16(len(c.t.outputs)), each: -1, coin: -1}
	if s.Index != nil {
		if *s.Index > math.MaxUint16 {
			return bad("output %q index out of range", s.Name)
		}
		o.index = uint16(*s.Index)
	}
	var err error
	if o.onchain, err = c.slotType(s.Type); err != nil {
		return err
	}
	for _, sd := range s.Value.Draws {
		var dr draw
		if dr.from, err = c.inputIndex(sd.From, "value"); err != nil {
			return err
		}
		if slices.ContainsFunc(o.draws, func(x draw) bool { return x.from == dr.from }) {
			return bad("output %q draws from input %q twice", o.name, sd.From)
		}
		if sd.Amount != nil {
			if dr.amount, err = c.amount(*sd.Amount); err != nil {
				return err
			}
		}
		o.draws = append(o.draws, dr)
	}
	o.from = o.draws[0].from
	if o.locking, err = c.locking(s.Locking); err != nil {
		return err
	}
	if len(s.Assets) > 0 && o.onchain {
		return bad("output %q cannot carry assets", o.name)
	}
	for i := range s.Assets {
		r, err := c.route(&s.Assets[i])
		if err != nil {
			return err
		}
		if slices.ContainsFunc(o.assets, r.sameAsset) {
			return bad("output %q routes an asset of an input twice", o.name)
		}
		o.assets = append(o.assets, r)
	}
	if s.Each != "" {
		if o.each, err = c.inputIndex(s.Each, "each"); err != nil {
			return err
		}
		if slices.ContainsFunc(o.draws, func(d draw) bool { return d.from != o.each }) ||
			slices.ContainsFunc(o.assets, func(r assetRoute) bool { return r.from != o.each }) {
			return bad("output %q repeats per coin of %q and takes from it alone", o.name, s.Each)
		}
	}
	c.t.outputs = append(c.t.outputs, o)
	return nil
}

func (c *compiler) locking(s schema.Locking) (locking, error) {
	l := locking{from: -1}
	var err error
	switch {
	case s.Script != nil:
		l.script, err = c.bind(*s.Script, "")
	case s.Contract != nil:
		l.contract, err = c.construction(s.Contract)
		if err == nil && l.contract.taptree != "" {
			err = bad("a contract checked against a given taptree cannot lock an output")
		}
	default:
		l.from, err = c.inputIndex(s.From, "locking")
	}
	return l, err
}

// checkPrograms: a program binding names a contract output, whose own bindings name no program.
func (t *Template) checkPrograms() error {
	for _, o := range t.outputs {
		if o.locking.contract != nil &&
			slices.ContainsFunc(slices.Collect(maps.Values(o.locking.contract.args)), hasProgram) {
			return bad("output %q binds an output program", o.name)
		}
	}
	for _, in := range t.inputs {
		for _, b := range in.contract.args {
			for _, pc := range b {
				if k := t.output(
					pc.program,
				); pc.program != "" &&
					(k < 0 || t.outputs[k].locking.contract == nil) {
					return bad("program %q names no output built from a contract", pc.program)
				}
			}
		}
	}
	return nil
}

// checkBalances: one remainder draw per input, one remainder route per input and asset.
func (t *Template) checkBalances() error {
	for i, in := range t.inputs {
		n := 0
		for _, o := range t.outputs {
			for _, dr := range o.draws {
				if dr.from == i && dr.amount == nil {
					n++
				}
			}
		}
		if n != 1 {
			return bad("input %q needs exactly one remainder draw, has %d", in.name, n)
		}
	}
	var remainders []assetRoute
	routed := make([]bool, len(t.issues))
	for _, o := range t.sinks() {
		for _, r := range o.routes {
			if r.issue >= 0 {
				routed[r.issue] = true
			}
			if r.amount != nil {
				continue
			}
			if slices.ContainsFunc(remainders, r.sameAsset) {
				return bad("input %q has two remainder routes for one asset", t.inputs[r.from].name)
			}
			remainders = append(remainders, r)
		}
	}
	if k := slices.Index(routed, false); k >= 0 {
		return bad("issue %q is routed to no output", t.issues[k].name)
	}
	return nil
}

// checkPlacement requires indexes 0..n-1 once, laid out as offchain outputs, extension, onchain
// outputs. Unindexed outputs take list order and the extension is placed when built.
func (t *Template) checkPlacement() error {
	if t.packets != nil && t.packets.index < 0 {
		if t.typ == Proof && !slices.IsSortedFunc(t.outputs, func(a, b output) int {
			return cmp.Compare(btoi(a.onchain), btoi(b.onchain))
		}) {
			return bad("a proof template lists its offchain outputs before its onchain ones")
		}
		return nil
	}
	var idx []int
	offchain := 0
	for _, o := range t.outputs {
		idx = append(idx, int(o.index))
		if !o.onchain {
			offchain++
		}
	}
	if t.packets != nil {
		idx = append(idx, int(t.packets.index))
	}
	slices.Sort(idx)
	for i, n := range idx {
		if n != i {
			return bad("output indexes must fill 0..%d once", len(idx)-1)
		}
	}
	if t.typ != Proof {
		return nil
	}
	for _, o := range t.outputs {
		if o.onchain == (int(o.index) < offchain) {
			return bad("an intent places offchain outputs, the extension, then onchain outputs")
		}
	}
	if t.packets != nil && int(t.packets.index) != offchain {
		return bad("an intent places its extension after the offchain outputs")
	}
	return nil
}

// allocate balances every input separately; fee comes out of fees.from and dust is the minimum
// output.
func (i *Instance) allocate(
	d *draft, sources []*Source, fee uint64, dust uint64,
) (*allocation, error) {
	t := i.tmpl
	feeCap := i.feeCap()
	switch {
	case fee > 0 && t.fee == nil:
		return nil, fmt.Errorf(
			"%w: %w: the template pays no fee, asked %d", ErrIneligible, ErrUnaffordable, fee,
		)
	case t.fee != nil && fee > feeCap:
		return nil, fmt.Errorf(
			"%w: %w: fee %d exceeds the cap %d", ErrIneligible, ErrUnaffordable, fee, feeCap,
		)
	case len(sources) != len(t.inputs):
		return nil, fmt.Errorf(
			"%w: %d sources for %d inputs", ErrIneligible, len(sources), len(t.inputs),
		)
	}
	if k := slices.Index(sources, nil); k >= 0 {
		return nil, fmt.Errorf("%w: input %q has no source", ErrIneligible, t.inputs[k].name)
	}
	a := &allocation{
		sats:   make([]uint64, len(t.outputs)),
		assets: make([]map[string]uint64, len(t.outputs)+len(t.burns)),
		fee:    fee,
	}
	for k := range a.assets {
		a.assets[k] = map[string]uint64{}
	}
	for _, coins := range t.groups {
		name := t.inputs[coins[0]].name
		pool, pooled := uint64(0), map[string]uint64{}
		for _, c := range coins {
			s := sources[c]
			if s.Amount > maxAmount {
				return nil, fmt.Errorf(
					"%w: input %q holds more than 21e14 satoshis", ErrIneligible, name,
				)
			}
			held := map[string]uint64{}
			for _, as := range s.Assets {
				if len(as.ID) != 34 {
					return nil, fmt.Errorf(
						"%w: input %q holds an asset id of %d bytes",
						ErrIneligible,
						name,
						len(as.ID),
					)
				}
				if err := credit(held, hex.EncodeToString(as.ID), as.Amount); err != nil {
					return nil, fmt.Errorf("input %q: %w", name, err)
				}
			}
			// the copies of an each output take from their own coin first
			left, err := i.routeSats(d, c, s.Amount, true, a)
			if err != nil {
				return nil, err
			}
			if err := i.routeAssets(d, c, held, a, true); err != nil {
				return nil, err
			}
			pool += left
			for key, n := range held {
				if err := credit(pooled, key, n); err != nil {
					return nil, fmt.Errorf("input %q: %w", name, err)
				}
			}
		}
		if _, err := i.routeSats(d, coins[0], pool, false, a); err != nil {
			return nil, err
		}
		if err := i.routeAssets(d, coins[0], pooled, a, false); err != nil {
			return nil, err
		}
		for key, n := range pooled {
			if n > 0 {
				return nil, fmt.Errorf(
					"%w: input %q has %d of asset %s without a route", ErrIneligible, name, n, key,
				)
			}
		}
	}
	if err := i.mint(d, a); err != nil {
		return nil, err
	}
	if err := i.checkDust(a, dust); err != nil {
		return nil, err
	}
	return a, nil
}

// routeSats serves the draws on slot of the copies of each outputs, or of the other outputs and
// the fee, and returns what is left.
func (i *Instance) routeSats(
	d *draft, slot int, amount uint64, copies bool, a *allocation,
) (uint64, error) {
	t := i.tmpl
	left := amount
	mine := func(o output, dr draw) bool { return dr.from == slot && (o.coin >= 0) == copies }
	for k, o := range t.outputs {
		for _, dr := range o.draws {
			if !mine(o, dr) || dr.amount == nil {
				continue
			}
			n, err := i.fixedAmount(dr.amount, d, o.from)
			if err != nil {
				return 0, fmt.Errorf("output %q: %w", o.name, err)
			}
			if n > left {
				return 0, fmt.Errorf(
					"%w: output %q draws %d of %d left",
					ErrIneligible,
					o.name,
					n,
					left,
				)
			}
			left -= n
			a.sats[k] += n
		}
	}
	if !copies && t.fee != nil && t.fee.from == slot {
		if a.fee > left {
			return 0, fmt.Errorf(
				"%w: input %q cannot pay fee %d", ErrIneligible, t.inputs[t.fee.from].name, a.fee,
			)
		}
		left -= a.fee
	}
	for k, o := range t.outputs {
		for _, dr := range o.draws {
			if mine(o, dr) && dr.amount == nil {
				a.sats[k] += left
				left = 0
			}
		}
	}
	return left, nil
}

// checkDust runs once every input has drawn: an output's value is the sum of its draws.
func (i *Instance) checkDust(a *allocation, dust uint64) error {
	for k, o := range i.tmpl.outputs {
		if a.sats[k] < dust {
			return fmt.Errorf(
				"%w: %w: output %q holds %d, minimum %d", ErrIneligible, ErrUnaffordable, o.name,
				a.sats[k], dust,
			)
		}
	}
	return nil
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// fixedAmount requires a positive amount.
func (i *Instance) fixedAmount(b byteTemplate, d *draft, slot int) (uint64, error) {
	raw, err := i.resolve(b, d, d.index(slot))
	if err != nil {
		if !errors.Is(err, ErrIneligible) {
			err = fmt.Errorf("%w: %w", ErrIneligible, err)
		}
		return 0, err
	}
	n, err := asm.ParseScriptNum(raw)
	if err != nil || n < 1 {
		return 0, fmt.Errorf(
			"%w: amount of %d bytes is not a positive number", ErrIneligible, len(raw),
		)
	}
	return uint64(n), nil
}

func (i *Instance) outputScripts(d *draft, sources []*Source) ([][]byte, error) {
	out := make([][]byte, len(i.tmpl.outputs))
	for k, o := range i.tmpl.outputs {
		var err error
		switch l := o.locking; {
		case l.from >= 0:
			out[k] = bytes.Clone(pkScript(sources[l.from]))
		case l.contract != nil:
			var ct *contract.Contract
			if ct, err = i.contractOf(l.contract, d, o.from, false); err == nil {
				out[k] = ct.PkScript
			}
		default:
			out[k], err = i.resolve(l.script, d, d.index(o.from))
			if err == nil {
				err = checkScript(out[k], o.onchain)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("output %q: %w", o.name, err)
		}
	}
	return out, nil
}

// credit leaves zero out of m.
func credit(m map[string]uint64, key string, n uint64) error {
	if n == 0 {
		return nil
	}
	sum, carry := bits.Add64(m[key], n, 0)
	if carry != 0 {
		return fmt.Errorf("%w: asset %s overflows", ErrIneligible, key)
	}
	m[key] = sum
	return nil
}

// checkScript requires P2TR off chain and a script that parses on chain.
func checkScript(s []byte, onchain bool) error {
	if !onchain {
		if !txscript.IsPayToTaproot(s) {
			return fmt.Errorf(
				"%w: an offchain output needs a P2TR script, got %d bytes", ErrIneligible, len(s),
			)
		}
		if _, err := schnorr.ParsePubKey(s[2:]); err != nil {
			return fmt.Errorf("%w: an offchain output needs a valid taproot key", ErrIneligible)
		}
		return nil
	}
	tok := txscript.MakeScriptTokenizer(0, s)
	for tok.Next() {
	}
	if len(s) == 0 || tok.Err() != nil {
		return fmt.Errorf("%w: malformed script of %d bytes", ErrIneligible, len(s))
	}
	return nil
}
