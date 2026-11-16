package v1

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/arkade-os/executor/pkg/template/artifact"

	"github.com/arkade-os/executor/pkg/template/internal/version/v1/schema"
)

type compiler struct {
	artifacts   map[string]*artifact.Artifact
	definitions map[string]*artifact.Definition
	doc         *schema.Template
	t           *Template
	named       map[string]bool // variables some binding uses
}

func (c *compiler) inputIndex(name, what string) (int, error) {
	i := slices.IndexFunc(c.t.inputs, func(in input) bool { return in.name == name })
	if i < 0 {
		return 0, bad("%s names no input: %q", what, name)
	}
	return i, nil
}

func (c *compiler) intVariable(v schema.IntOrVar) (int64, string, error) {
	if v.Int != nil {
		return *v.Int, "", nil
	}
	if c.t.variables[v.Var] != "int" {
		return 0, "", bad("%q is not an int variable", "<"+v.Var+">")
	}
	return 0, v.Var, nil
}

// slotType resolves a slot's type, which defaults to the template's and may differ only in a proof.
func (c *compiler) slotType(given string) (bool, error) {
	def := Virtual
	if c.t.typ == Onchain {
		def = Onchain
	}
	typ := def
	if given != "" {
		typ = Type(given)
	}
	if c.t.typ != Proof && typ != def {
		return false, bad("%s slot in a template of type %s", typ, c.t.typ)
	}
	return typ == Onchain, nil
}

// confineTaptrees: a taptree variable escapes the push size cap, so it fills nothing else.
func (c *compiler) confineTaptrees() error {
	for _, in := range c.t.inputs {
		if v := in.contract.taptree; v != "" && c.named[v] {
			return bad("taptree variable %q is used outside the taptree field", v)
		}
	}
	return nil
}

func compile(doc *schema.Template, artifacts map[string]*artifact.Artifact) (*Template, error) {
	c := &compiler{
		artifacts:   artifacts,
		definitions: map[string]*artifact.Definition{},
		doc:         doc,
		named:       map[string]bool{},
		t: &Template{
			typ:       Type(doc.Type),
			variables: map[string]string{},
			sealed:    map[string]bool{},
		},
	}
	if err := unsupported(doc); err != nil {
		return nil, err
	}
	if err := c.declarations(); err != nil {
		return nil, err
	}
	inputs, err := inTxOrder(doc.Inputs)
	if err != nil {
		return nil, err
	}
	for i := range inputs {
		if err := c.input(&inputs[i]); err != nil {
			return nil, err
		}
	}
	if err := c.issues(); err != nil {
		return nil, err
	}
	for i := range doc.Outputs {
		if err := c.output(&doc.Outputs[i]); err != nil {
			return nil, err
		}
	}
	for i := range doc.Burns {
		r, err := c.route(&doc.Burns[i])
		if err != nil {
			return nil, err
		}
		c.t.burns = append(c.t.burns, r)
	}
	for _, check := range []func() error{
		c.schedules, c.t.checkPrograms, c.t.checkBalances, c.fees, c.packets, c.triggers,
		c.t.checkPlacement, c.confineTaptrees, c.locks,
	} {
		if err := check(); err != nil {
			return nil, err
		}
	}
	return c.t, nil
}

// unsupported refuses sealing schemes other than the default, the only one defined.
func unsupported(doc *schema.Template) error {
	d := doc.Delegation
	if d == nil {
		return nil
	}
	for _, x := range d.Sealed {
		if x.Scheme != "" {
			return fmt.Errorf("%w: sealed scheme %s is not supported", ErrUnsupported, x.Scheme)
		}
	}
	all := slices.ContainsFunc(
		slices.Collect(maps.Values(d.Trigger)),
		func(tr schema.Trigger) bool {
			return tr.AllOutputs
		},
	)
	if all &&
		(slices.ContainsFunc(doc.Inputs, func(in schema.Input) bool { return in.Index != nil }) ||
			slices.ContainsFunc(doc.Outputs, func(o schema.Output) bool { return o.Index != nil })) {
		return bad("no entry has an index when a rule sets all_outputs")
	}
	return nil
}

// inTxOrder sorts indexed inputs by index, which must fill 0..n-1 once; slots are tx positions.
func inTxOrder(in []schema.Input) ([]schema.Input, error) {
	out := slices.Clone(in)
	if out[0].Index == nil {
		return out, nil
	}
	slices.SortFunc(out, func(a, b schema.Input) int { return cmp.Compare(*a.Index, *b.Index) })
	for i, x := range out {
		if *x.Index != int64(i) {
			return nil, bad("input indexes must fill 0..%d once", len(out)-1)
		}
	}
	return out, nil
}
