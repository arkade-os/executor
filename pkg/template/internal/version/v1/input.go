package v1

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/arkade-os/arkd/pkg/ark-lib/script"
	"github.com/arkade-os/executor/pkg/template/artifact"
	"github.com/arkade-os/executor/pkg/template/internal/asm"
	"github.com/arkade-os/executor/pkg/template/internal/contract"
	"github.com/arkade-os/executor/pkg/template/internal/failure"
	"github.com/arkade-os/executor/pkg/template/internal/model"
	"github.com/arkade-os/executor/pkg/template/internal/version/v1/schema"
)

type construction struct {
	def     *artifact.Definition
	args    map[string]byteTemplate // flattened constructor name → binding
	taptree string                  // bytes variable holding the whole tree; "" builds it from def
}

type input struct {
	selected       *artifact.Function
	selectedLeaf   *artifact.Leaf
	name           string
	onchain        bool
	schedule       model.Schedule
	leadVariable   string // an int variable holding before_expiry_seconds
	contract       construction
	function, leaf string
	arguments      []argument              // flattened covenant inputs, ABI order
	witness        map[string]byteTemplate // non-signature witness items
	sequence       *lockSpec               // stated nSequence, nil to derive it from the leaf
}

type argument struct {
	field artifact.Field
	value byteTemplate
}

// lockSpec is a literal or an int variable.
type lockSpec struct {
	n        int64
	variable string
}

// Locktime is the absolute locktime of slot's spend leaf, which arkd compares with its chain tip.
func (i *Instance) Locktime(slot int) (arklib.AbsoluteLocktime, bool) {
	slot = i.firstCoin(slot)
	if lt := i.locktimes[slot]; lt != nil {
		return *lt, true
	}
	return 0, false
}

func (i *Instance) Tapscripts(
	slot int,
) []string {
	return slices.Clone(i.contracts[i.firstCoin(slot)].Tapscripts)
}

func (i *Instance) PkScript(slot int) []byte {
	return bytes.Clone(i.contracts[i.firstCoin(slot)].PkScript)
}

// DueAt is the schedule's due time, no earlier than a timestamp locktime or a relative lock in
// seconds; heights are the caller's to check.
func (i *Instance) DueAt(slot int, s *model.Source, now time.Time) time.Time {
	slot = i.firstCoin(slot)
	due := i.scheduleDue(slot, s, now)
	if i.locktime >= locktimeThreshold {
		due = later(due, time.Unix(int64(i.locktime), 0))
	}
	if seq := i.sequences[slot]; seq&seqDisabled == 0 && seq&seqSeconds != 0 &&
		!s.CreatedAt.IsZero() {
		due = later(due, s.CreatedAt.Add(time.Duration(seq&seqMask)*512*time.Second))
	}
	return due
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

func (c *compiler) input(s *schema.Input) error {
	if slices.ContainsFunc(c.t.inputs, func(o input) bool { return o.name == s.Name }) {
		return bad("duplicate input %q", s.Name)
	}
	in := input{name: s.Name}
	var err error
	if in.onchain, err = c.slotType(s.Type); err != nil {
		return err
	}
	ct, err := c.construction(&s.Contract)
	if err != nil {
		return err
	}
	in.contract = *ct
	if err := c.spend(&in, &s.Spend); err != nil {
		return err
	}
	c.t.inputs = append(c.t.inputs, in)
	return nil
}

func (c *compiler) schedule(s *schema.Schedule, onchain bool) (model.Schedule, string, error) {
	switch {
	case s.Immediate != nil:
		return model.Schedule{}, "", nil
	case s.BeforeExpiry != nil && !onchain:
		n, name, err := c.intVariable(*s.BeforeExpiry)
		if err != nil || name != "" {
			return model.Schedule{}, name, err
		}
		return model.Schedule{BeforeExpiry: time.Duration(n) * time.Second}, "", nil
	case s.MinConfirmations != nil && onchain:
		return model.Schedule{MinConfirmations: int(*s.MinConfirmations)}, "", nil
	}
	return model.Schedule{}, "", bad("the schedule does not fit the input")
}

func (c *compiler) construction(s *schema.Construction) (*construction, error) {
	def, err := c.definition(s.Definition)
	if err != nil {
		return nil, err
	}
	c.t.requiredKeys |= keysRequired(def)
	flat, err := def.FlatConstructor()
	if err != nil {
		return nil, err
	}
	ct := &construction{def: def, args: map[string]byteTemplate{}}
	if s.Taptree != "" {
		name := s.Taptree[1 : len(s.Taptree)-1] // <name>, checked by the schema
		if c.t.variables[name] != "bytes" {
			return nil, bad("taptree must name a bytes variable, got %q", s.Taptree)
		}
		ct.taptree = name
	}
	for _, f := range flat {
		raw, ok := s.Arguments[f.Name]
		if !ok {
			return nil, bad("missing argument %q", f.Name)
		}
		if raw.Program != nil {
			if f.Type != "bytes32" {
				return nil, bad("output program %q cannot fill a %s field", *raw.Program, f.Type)
			}
			ct.args[f.Name] = byteTemplate{{program: *raw.Program}}
			continue
		}
		if ct.args[f.Name], err = c.scriptBind(raw, f.Type); err != nil {
			return nil, err
		}
	}
	if len(s.Arguments) != len(flat) {
		return nil, bad("contract %q has arguments outside its constructor", def.ContractName)
	}
	return ct, nil
}

func (c *compiler) definition(ref schema.DefinitionRef) (*artifact.Definition, error) {
	if d := ref.Def; d != nil {
		return d, nil
	}
	id := ref.Artifact
	if def, ok := c.definitions[id]; ok {
		return def, nil
	}
	a := c.artifacts[id]
	if a == nil {
		return nil, bad("artifact %s not provided", id)
	}
	if a.Version() != artifact.Version1 {
		return nil, fmt.Errorf("%w: artifact profile %q", failure.ErrUnsupported, a.Version())
	}
	if a.ID() != id {
		return nil, bad("artifact %s resolves to a document with id %s", id, a.ID())
	}
	def := a.Definition()
	c.definitions[id] = &def
	c.t.artifacts = append(c.t.artifacts, id)
	return &def, nil
}

func (c *compiler) spend(in *input, s *schema.Spend) error {
	f := in.contract.def.FindFunction(s.Function)
	if f == nil {
		return bad("unknown function %q", s.Function)
	}
	l := f.FindLeaf(s.Leaf)
	if l == nil {
		return bad("unknown leaf %q", s.Leaf)
	}
	// an intent proof carries no locktime
	if slices.ContainsFunc(l.Asm, isCLTV) && c.t.typ == model.Proof {
		return fmt.Errorf(
			"%w: leaf %q needs an absolute locktime, which a proof does not set",
			failure.ErrUnsupported, l.Name,
		)
	}
	in.function, in.leaf = f.Name, l.Name
	in.selected, in.selectedLeaf = f, l
	var want []artifact.Field
	if f.Arkade != nil {
		var err error
		if want, err = in.contract.def.Flatten(f.Arkade.Inputs); err != nil {
			return err
		}
	}
	if len(s.Arguments) != len(want) {
		return bad("function %q takes %d arguments, got %d", f.Name, len(want), len(s.Arguments))
	}
	for i, a := range s.Arguments {
		b, err := c.bind(a, want[i].Type)
		if err != nil {
			return err
		}
		in.arguments = append(in.arguments, argument{field: want[i], value: b})
	}
	in.witness = map[string]byteTemplate{}
	for _, w := range l.Witness {
		if w.Type == "signature" {
			if !w.Injected {
				return bad("leaf %q needs signature %q, which nobody injects", l.Name, w.Name)
			}
			continue
		}
		raw, ok := s.Witness[w.Name]
		if !ok {
			return bad("witness item %q is not bound", w.Name)
		}
		var err error
		if in.witness[w.Name], err = c.bind(raw, w.Type); err != nil {
			return err
		}
	}
	if len(s.Witness) != len(in.witness) {
		return bad("spend binds witness items that are signatures or absent from leaf %q", l.Name)
	}
	return nil
}

// a non-canonical value is ineligible, unless watch, where it can only come from the variables
func (i *Instance) contractOf(
	c *construction, d *draft, slot int, watch bool,
) (*contract.Contract, error) {
	flat, err := c.def.FlatConstructor()
	if err != nil {
		return nil, err
	}
	args := map[string]asm.Value{}
	for _, f := range flat {
		raw, err := i.resolve(c.args[f.Name], d, d.index(slot))
		if err == nil {
			if err = asm.CanonicalBytes(f.Type, raw); err != nil {
				sentinel := failure.ErrIneligible
				if watch {
					sentinel = failure.ErrInvalidVariables
				}
				err = fmt.Errorf("%w: %v", sentinel, err)
			}
		}
		if err != nil {
			return nil, fmt.Errorf("argument %q: %w", f.Name, err)
		}
		args[f.Name] = asm.Value{Type: f.Type, Raw: raw}
	}
	var taptree []byte
	if c.taptree != "" {
		taptree = append([]byte{}, i.vars[c.taptree]...) // non-nil: a given tree, empty or not
	}
	ct, err := contract.Build(c.def, args, i.keys, taptree)
	return ct, contractErr(err)
}

// lead is how long before expiry the input becomes due, zero without an expiry schedule.
func (i *Instance) lead(slot int) time.Duration {
	in := i.tmpl.inputs[slot]
	if in.leadVariable == "" {
		return in.schedule.BeforeExpiry
	}
	n, _ := asm.ParseScriptNum(i.vars[in.leadVariable])
	return time.Duration(n) * time.Second
}

// scheduleDue is the window start for an expiry lead, else the source's creation, or now when
// unknown. A fee-paying instance waits at least half the source's life: a longer lead would pay in
// every batch.
func (i *Instance) scheduleDue(slot int, s *model.Source, now time.Time) time.Time {
	sch := i.tmpl.inputs[slot].schedule
	lead := i.lead(slot)
	if i.feeCap() > 0 && !s.CreatedAt.IsZero() {
		if life := s.Expiry.Sub(s.CreatedAt); life > 0 {
			lead = min(lead, life/2)
		}
	}
	switch {
	case lead > 0 && !s.Expiry.IsZero():
		return s.Expiry.Add(-lead)
	case lead > 0, s.Confirms < sch.MinConfirmations:
		return now.Add(time.Hour)
	case !s.CreatedAt.IsZero():
		return s.CreatedAt
	}
	return now
}

func contractErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, contract.ErrNoCosigner):
		return fmt.Errorf("%w: %v", failure.ErrUnsupported, err)
	case errors.Is(err, contract.ErrBadTree):
		return fmt.Errorf("%w: %v", failure.ErrIneligible, err)
	case errors.Is(err, contract.ErrBadLeaf):
		return fmt.Errorf("%w: %v", failure.ErrInvalidTemplate, err)
	}
	return err
}

func isCLTV(tok string) bool { return tok == "OP_CHECKLOCKTIMEVERIFY" || tok == "OP_NOP2" }

// checkClosure returns a virtual spend leaf's absolute locktime, nil without one. The emulator
// accepts only ark-lib closures on a leaf naming its key, and arkd builds a locktime only from a
// CLTV multisig.
func checkClosure(
	in input,
	ct *contract.Contract,
	typ model.Type,
) (*arklib.AbsoluteLocktime, error) {
	f := in.contract.def.FindFunction(in.function)
	l := f.FindLeaf(in.leaf)
	emulated := slices.ContainsFunc(l.Asm, func(tok string) bool {
		return strings.HasPrefix(tok, "<EMULATOR_KEY:") || strings.HasPrefix(tok, "<TWEAK:")
	})
	cltv := typ == model.Virtual && slices.ContainsFunc(l.Asm, isCLTV)
	if !emulated && !cltv {
		return nil, nil
	}
	c, err := script.DecodeClosure(ct.Proofs[[2]string{in.function, in.leaf}].Script)
	lock, isLock := c.(*script.CLTVMultisigClosure)
	switch {
	case cltv && !isLock:
		return nil, fmt.Errorf(
			"%w: input %q: leaf %q is not a CLTV multisig closure", failure.ErrUnsupported, in.name,
			in.leaf,
		)
	case err != nil:
		return nil, fmt.Errorf(
			"%w: input %q: leaf %q is not a closure", failure.ErrUnsupported, in.name, in.leaf,
		)
	case cltv:
		return &lock.Locktime, nil
	}
	return nil, nil
}

func sameDomain(lts []*arklib.AbsoluteLocktime) bool {
	var first *arklib.AbsoluteLocktime
	for _, lt := range lts {
		if lt == nil {
			continue
		}
		if first != nil && first.IsSeconds() != lt.IsSeconds() {
			return false
		}
		first = lt
	}
	return true
}

func checkSourceScript(in input, s *model.Source, ct *contract.Contract) error {
	if !bytes.Equal(pkScript(s), ct.PkScript) {
		return fmt.Errorf(
			"%w: input %q: source script differs from the contract", failure.ErrIneligible, in.name,
		)
	}
	return nil
}
