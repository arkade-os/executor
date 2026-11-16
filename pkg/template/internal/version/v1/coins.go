package v1

import (
	"fmt"
	"slices"
)

// expand lays out an instance: an input becomes a slot per coin and an output with each a copy per
// coin of its input. Other references to an input name its first coin.
func (t *Template) expand(counts []int) (*Template, error) {
	if counts != nil && len(counts) != len(t.inputs) {
		return nil, fmt.Errorf(
			"%w: %d coin counts for %d inputs",
			ErrIneligible,
			len(counts),
			len(t.inputs),
		)
	}
	x := *t
	x.inputs, x.outputs, x.groups = nil, nil, make([][]int, len(t.inputs))
	for s, in := range t.inputs {
		n := 1
		if counts != nil {
			n = counts[s]
		}
		if n < 1 || n > 1 && !t.allOutputs(s) {
			return nil, fmt.Errorf("%w: input %q cannot hold %d coins", ErrIneligible, in.name, n)
		}
		for range n {
			x.groups[s] = append(x.groups[s], len(x.inputs))
			x.inputs = append(x.inputs, in)
		}
	}
	first := func(s int) int {
		if s < 0 {
			return s
		}
		return x.groups[s][0]
	}
	copies := make([][]int, len(t.outputs))
	for k, o := range t.outputs {
		coins := []int{-1}
		if o.each >= 0 {
			coins = x.groups[o.each]
		}
		for _, c := range coins {
			to := func(s int) int {
				if c >= 0 && s == o.each {
					return c
				}
				return first(s)
			}
			y := o
			y.coin, y.from, y.locking.from = c, to(o.from), to(o.locking.from)
			y.draws = slices.Clone(o.draws)
			for j := range y.draws {
				y.draws[j].from = to(y.draws[j].from)
			}
			y.assets = slices.Clone(o.assets)
			for j := range y.assets {
				y.assets[j].from = to(y.assets[j].from)
			}
			copies[k] = append(copies[k], len(x.outputs))
			x.outputs = append(x.outputs, y)
		}
	}
	if len(x.inputs) != len(t.inputs) || len(x.outputs) != len(t.outputs) {
		for k := range x.outputs {
			x.outputs[k].index = uint16(k) // all_outputs leaves no index
		}
	}
	x.burns = slices.Clone(t.burns)
	for j := range x.burns {
		x.burns[j].from = first(x.burns[j].from)
	}
	if t.fee != nil {
		f := *t.fee
		f.from = first(f.from)
		x.fee = &f
	}
	if t.packets != nil {
		p := *t.packets
		p.rules = slices.Clone(p.rules)
		for j := range p.rules {
			p.rules[j].from = first(p.rules[j].from)
		}
		p.adverts = slices.Clone(p.adverts)
		for j, ad := range p.adverts {
			p.adverts[j].outputs = nil
			for _, k := range ad.outputs {
				p.adverts[j].outputs = append(p.adverts[j].outputs, copies[k]...)
			}
		}
		x.packets = &p
	}
	return &x, nil
}

func (t *Template) allOutputs(slot int) bool {
	return slices.ContainsFunc(
		t.triggers,
		func(tr *Trigger) bool { return tr.input == slot && tr.all },
	)
}

// firstCoin maps an input of the template to its first slot in the instance.
func (i *Instance) firstCoin(slot int) int {
	if slot < 0 || slot >= len(i.tmpl.groups) {
		return len(i.tmpl.inputs) // out of range, as the caller's index was
	}
	return i.tmpl.groups[slot][0]
}

func (i *Instance) relayout(coins []int) (*Instance, error) {
	if !i.watch {
		return nil, fmt.Errorf("%w: only a watch takes a new coin layout", ErrUnsupported)
	}
	x, err := i.base.expand(coins)
	if err != nil {
		return nil, err
	}
	j := *i
	j.tmpl, j.contracts, j.locktimes, j.sequences = x, nil, nil, nil
	for s, g := range x.groups {
		k := i.firstCoin(s)
		for range g {
			j.contracts = append(j.contracts, i.contracts[k])
			j.locktimes = append(j.locktimes, i.locktimes[k])
			j.sequences = append(j.sequences, i.sequences[k])
		}
	}
	return &j, nil
}
