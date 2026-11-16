package v1

import (
	"bytes"
	"fmt"
	"maps"
	"math"
	"slices"

	"github.com/arkade-os/executor/pkg/template/internal/model"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/arkade-os/executor/pkg/template/internal/asm"
	"github.com/arkade-os/executor/pkg/template/internal/contract"
)

type Keys = model.Keys
type Context = model.Context

// Instance is a template bound to keys, variables and, unless it is a watch, sources.
type Instance struct {
	base      *Template // before expansion
	watch     bool
	tmpl      *Template
	keys      Keys
	vars      map[string][]byte
	contracts []*contract.Contract       // per slot
	locktimes []*arklib.AbsoluteLocktime // per slot, nil without a CLTV spend leaf
	locktime  uint32                     // the transaction's nLockTime
	sequences []uint32                   // per slot
}

func (t *Template) Instantiate(c Context) (*Instance, error) {
	vars, err := t.open(c.Variables, c.Decrypt)
	if err != nil {
		return nil, err
	}
	if err := t.validateVariables(vars, false); err != nil {
		return nil, err
	}
	watch := c.Sources == nil
	sources := c.Sources
	coins := c.Coins
	if watch {
		coins = nil
	}
	x, err := t.expand(coins)
	if err != nil {
		return nil, err
	}
	if watch {
		if !t.ContextFree() {
			return nil, fmt.Errorf(
				"%w: a watch needs context-free contract arguments", ErrUnsupported,
			)
		}
		sources = make([]*Source, len(x.inputs))
		for slot := range sources {
			sources[slot] = &Source{}
		}
	} else if err := checkSources(sources, len(x.inputs)); err != nil {
		return nil, err
	}
	d := newDraft(t.typ, sources)
	d.contextFree = watch

	i := &Instance{base: t, watch: watch, tmpl: x, keys: c.Keys, vars: map[string][]byte{}}
	var csvs []*uint32
	for n, v := range vars {
		i.vars[n] = bytes.Clone(v)
	}
	for slot, in := range x.inputs {
		ct, err := i.contractOf(&in.contract, d, slot, watch)
		if err != nil {
			return nil, fmt.Errorf("input %q: %w", in.name, err)
		}
		lt, err := checkClosure(in, ct, t.typ)
		if err != nil {
			return nil, err
		}
		cltv, csv := leafLocks(ct.Proofs[[2]string{in.function, in.leaf}].Script)
		if lt == nil && cltv != nil {
			v := arklib.AbsoluteLocktime(*cltv)
			lt = &v
		}
		i.locktimes = append(i.locktimes, lt)
		csvs = append(csvs, csv)
		if !watch {
			if err := checkSourceScript(in, sources[slot], ct); err != nil {
				return nil, err
			}
		}
		i.contracts = append(i.contracts, ct)
	}
	if !sameDomain(i.locktimes) {
		return nil, fmt.Errorf("%w: locktimes mix block heights and timestamps", ErrUnsupported)
	}
	if err := i.resolveLocks(csvs); err != nil {
		return nil, err
	}
	return i, nil
}

func (t *Template) checkRanges(vars map[string][]byte, skipSealed bool) error {
	check := func(name string, lo, hi int64) error {
		if skipSealed && t.sealed[name] {
			return nil
		}
		if n, err := asm.ParseScriptNum(vars[name]); err != nil || n < lo || n > hi {
			return fmt.Errorf("%w: %q must be in %d..%d", ErrInvalidVariables, name, lo, hi)
		}
		return nil
	}
	for _, in := range t.inputs {
		if in.leadVariable != "" {
			if err := check(in.leadVariable, 1, math.MaxUint32); err != nil {
				return err
			}
		}
	}
	locks := []*lockSpec{t.locktime}
	for _, in := range t.inputs {
		locks = append(locks, in.sequence)
	}
	for _, l := range locks {
		if l != nil && l.variable != "" {
			if err := check(l.variable, 0, math.MaxUint32); err != nil {
				return err
			}
		}
	}
	if t.fee != nil && t.fee.maxVariable != "" {
		return check(t.fee.maxVariable, 0, asm.MaxScriptNum)
	}
	return nil
}

func (t *Template) validateVariables(vars map[string][]byte, skipSealed bool) error {
	if len(vars) != len(t.variables) {
		return fmt.Errorf(
			"%w: %d values for %d variables", ErrInvalidVariables, len(vars), len(t.variables),
		)
	}
	for _, n := range slices.Sorted(maps.Keys(t.variables)) {
		typ := t.variables[n]
		v, ok := vars[n]
		if !ok {
			return fmt.Errorf("%w: missing %q", ErrInvalidVariables, n)
		}
		if t.isTaptree(n) || skipSealed && t.sealed[n] {
			continue
		}
		if err := asm.CanonicalBytes(typ, v); err != nil {
			return fmt.Errorf("%w: variable %q: %v", ErrInvalidVariables, n, err)
		}
	}
	if err := t.checkRanges(vars, skipSealed); err != nil {
		return err
	}

	return nil
}

func checkSources(sources []*Source, n int) error {
	if len(sources) != n {
		return fmt.Errorf("%w: %d sources for %d inputs", ErrIneligible, len(sources), n)
	}
	for i, s := range sources {
		if s == nil || s.Tx == nil {
			return fmt.Errorf("%w: source %d or its transaction missing", ErrIneligible, i)
		}
		for _, in := range s.Tx.TxIn {
			if in == nil {
				return fmt.Errorf("%w: source %d has a nil transaction input", ErrIneligible, i)
			}
		}
		for _, out := range s.Tx.TxOut {
			if out == nil {
				return fmt.Errorf("%w: source %d has a nil transaction output", ErrIneligible, i)
			}
		}
		if !isBound(s) {
			return fmt.Errorf(
				"%w: source %d is not the output of its transaction", ErrIneligible, i,
			)
		}
		if slices.ContainsFunc(
			sources[:i], func(o *Source) bool { return o.Outpoint == s.Outpoint },
		) {
			return fmt.Errorf("%w: source %d repeats an outpoint", ErrIneligible, i)
		}
	}
	return nil
}
