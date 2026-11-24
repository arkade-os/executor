// Package schema is the typed form of a template document: its shape, its syntactic rules,
// its defaults and its canonical bytes. Anything that relates one part of the document to
// another is the compiler's job, not this package's.
package schema

import (
	"fmt"
	"math"
	"regexp"
	"slices"

	"github.com/arkade-os/executor/pkg/template/internal/asm"
	"github.com/arkade-os/executor/pkg/template/internal/failure"
)

const format = "template/v1"

var (
	// Name matches an identifier: slot, variable and secret names.
	Name       = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	idHex      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	namedVar   = regexp.MustCompile(`^<([a-z][a-z0-9_]*)>$`)
	types      = []string{"proof", "virtual", "onchain"}
	slotTypes  = []string{"offchain", "onchain"}
	actions    = []string{"copy", "drop", "emit"}
	unmatcheds = []string{"reject", "drop"}
	decodes    = []string{"tlv", "records", "raw"}
)

// Template is a whole document. Presence and null rules are specified by the typed readers.
type Template struct {
	Format     string            `json:"format"`
	Type       string            `json:"type"`                // proof, virtual or onchain
	Variables  map[string]string `json:"variables,omitempty"` // name → scalar type
	Inputs     []Input           `json:"inputs"`              // at least one
	Outputs    []Output          `json:"outputs"`             // at least one
	Fees       *Fees             `json:"fees,omitempty"`
	Issues     []Issue           `json:"issues,omitempty"`
	Burns      []AssetRoute      `json:"burns,omitempty"`
	Locktime   *IntOrVar         `json:"locktime,omitempty"`
	Packets    *Packets          `json:"packets,omitempty"`
	Delegation *Delegation       `json:"delegation,omitempty"`
}

// Delegation is how a service runs the spend unattended; it never changes the transaction.
type Delegation struct {
	Schedule map[string]Schedule `json:"schedule,omitempty"` // input → when it becomes eligible
	Sealed   []Sealed            `json:"sealed,omitempty"`   // variables given encrypted to the service
	Trigger  map[string]Trigger  `json:"trigger,omitempty"`  // input → stream rule
}

// Sealed is a sealed variable and its scheme, "" for the default one.
type Sealed struct {
	Name   string
	Scheme string
}

type Input struct {
	Name     string       `json:"name"`
	Type     string       `json:"type,omitempty"` // offchain or onchain; defaults to the template's kind
	Index    *int64       `json:"index,omitempty"`
	Sequence *IntOrVar    `json:"sequence,omitempty"`
	Contract Construction `json:"contract"`
	Spend    Spend        `json:"spend"`
}

// Schedule is when an input becomes eligible; exactly one field is set.
type Schedule struct {
	Immediate        *bool     `json:"immediate,omitempty"`             // true
	BeforeExpiry     *IntOrVar `json:"before_expiry_seconds,omitempty"` // positive or an int variable
	MinConfirmations *int64    `json:"min_confirmations,omitempty"`     // positive
}

type Construction struct {
	Definition DefinitionRef      `json:"definition"`
	Arguments  map[string]Binding `json:"arguments,omitempty"` // flattened constructor field → binding
	Taptree    string             `json:"taptree,omitempty"`   // <bytes variable> holding the whole tree
}

// Spend names the leaf an input is spent through and binds what the leaf needs.
type Spend struct {
	Function  string             `json:"function"`
	Leaf      string             `json:"leaf"`
	Arguments []Binding          `json:"arguments,omitempty"` // covenant inputs, ABI order
	Witness   map[string]Binding `json:"witness,omitempty"`   // non-signature witness items
}

type Output struct {
	Name    string       `json:"name"`
	Type    string       `json:"type,omitempty"` // offchain or onchain; defaults to the template's kind
	Index   *int64       `json:"index,omitempty"`
	Each    string       `json:"each,omitempty"` // an input
	Value   Value        `json:"value"`
	Locking Locking      `json:"locking"`
	Assets  []AssetRoute `json:"assets,omitempty"`
}

// Value is one draw, or a list of draws when List.
type Value struct {
	Draws []Draw
	List  bool
}

type Draw struct {
	From   string   `json:"from"`             // an input name
	Amount *Binding `json:"amount,omitempty"` // an int; absent takes the remainder
}

// AssetRoute moves an asset of an input into an output, or units an issue mints when Issue is set.
type AssetRoute struct {
	From   string   `json:"from,omitempty"`   // an input name
	Issue  string   `json:"issue,omitempty"`  // an issue name
	Asset  string   `json:"asset,omitempty"`  // byte template of an asset id; absent routes every asset
	Amount *Binding `json:"amount,omitempty"` // an int, only with Asset or Issue; absent takes the remainder
}

// Issue is an asset the transaction mints: new, or more of Asset.
type Issue struct {
	Name     string            `json:"name"`
	Asset    string            `json:"asset,omitempty"`   // byte template
	Control  *Control          `json:"control,omitempty"` // new assets only
	Metadata map[string]string `json:"metadata,omitempty"`
}

// Control is an asset id byte template, or another issue of the transaction.
type Control struct {
	Asset string
	Issue string
}

type Fees struct {
	From string   `json:"from"` // an input name
	Max  IntOrVar `json:"max"`  // 0..MaxScriptNum or an int variable
}

// Packets is the extension output and how source packets are handled.
type Packets struct {
	OutputIndex *int64   `json:"output_index,omitempty"` // only with indexed outputs
	Unmatched   string   `json:"unmatched,omitempty"`    // reject (default) or drop
	Rules       []Rule   `json:"rules,omitempty"`
	Advertise   []Advert `json:"advertise,omitempty"`
}

// Rule handles one packet type of one input.
type Rule struct {
	Type   int64    `json:"type"`           // 2..255, 5 reserved
	Action string   `json:"action"`         // copy, drop or emit
	From   string   `json:"from"`           // an input name
	Data   *Binding `json:"data,omitempty"` // the body, with emit only
}

// Advert names the template that claims outputs.
type Advert struct {
	Template string   `json:"template"` // self or a template id
	Outputs  []string `json:"outputs"`  // output names, non-empty
}

// Trigger is the stream rule of one input: it selects transactions by a packet and captures
// variables.
type Trigger struct {
	Packet       int64             `json:"packet"`                // 0..255
	Decode       string            `json:"decode,omitempty"`      // tlv (default), records or raw
	MinValue     int64             `json:"min_value,omitempty"`   // 0 is the default
	AllOutputs   bool              `json:"all_outputs,omitempty"` // true only
	Require      map[string]string `json:"require"`               // source → literal, key, <variable> or $(expression); non-empty
	Capture      map[string]string `json:"capture"`               // variable → source, output:taptree or $(expression); non-empty
	Group        string            `json:"group,omitempty"`       // a source; exactly when there are several rules
	ExpiresAfter int64             `json:"expires_after_seconds"` // positive
}

func (t *Template) check() error {
	if t.Format != format {
		return bad("format must be %q", format)
	}
	if !slices.Contains(types, t.Type) {
		return bad("unknown type %q", t.Type)
	}
	for _, n := range sortedKeys(t.Variables) {
		typ := t.Variables[n]
		if !Name.MatchString(n) {
			return bad("bad variable name %q", n)
		}
		if !slices.Contains(asm.ScalarTypes, typ) {
			return bad("variable %q has unsupported type %q", n, typ)
		}
	}
	if len(t.Inputs) == 0 || len(t.Outputs) == 0 {
		return bad("a template has at least one input and one output")
	}
	if max(len(t.Inputs), len(t.Outputs)) > math.MaxUint16 {
		return bad("a template has at most %d inputs or outputs", math.MaxUint16)
	}
	if err := allOrNone("input", t.Inputs, func(in Input) *int64 { return in.Index }); err != nil {
		return err
	}
	if err := allOrNone("output", t.Outputs, func(o Output) *int64 { return o.Index }); err != nil {
		return err
	}
	for _, in := range t.Inputs {
		if err := in.check(); err != nil {
			return err
		}
	}
	for _, o := range t.Outputs {
		if err := o.check(); err != nil {
			return err
		}
	}
	for _, is := range t.Issues {
		if err := is.check(); err != nil {
			return err
		}
	}
	for _, b := range t.Burns {
		if err := b.check(); err != nil {
			return err
		}
		if b.Issue != "" {
			return bad("a burn takes from an input")
		}
	}
	if t.Locktime != nil {
		if err := t.Locktime.check("locktime", 0, math.MaxUint32); err != nil {
			return err
		}
	}
	if t.Fees != nil {
		if err := t.Fees.Max.check("fees max", 0, math.MaxInt64); err != nil {
			return err
		}
	}
	if t.Packets != nil {
		if err := t.Packets.check(); err != nil {
			return err
		}
	}
	if t.Delegation != nil {
		return t.Delegation.check(t.Inputs)
	}
	return nil
}

// allOrNone: every entry of a list states an index, or none does.
func allOrNone[T any](what string, list []T, index func(T) *int64) error {
	n := 0
	for _, x := range list {
		if i := index(x); i != nil {
			if *i < 0 {
				return bad("%s index must not be negative", what)
			}
			n++
		}
	}
	if n != 0 && n != len(list) {
		return bad("every %s states an index or none does", what)
	}
	return nil
}

// check runs before normalization, which drops immediate schedules, so their keys are checked here.
func (d *Delegation) check(inputs []Input) error {
	for _, n := range append(sortedKeys(d.Schedule), sortedKeys(d.Trigger)...) {
		if !slices.ContainsFunc(inputs, func(in Input) bool { return in.Name == n }) {
			return bad("delegation names no input %q", n)
		}
	}
	for _, n := range sortedKeys(d.Schedule) {
		s := d.Schedule[n]
		if err := s.check(); err != nil {
			return err
		}
	}
	for i, x := range d.Sealed {
		if !Name.MatchString(x.Name) {
			return bad("bad sealed name %q", x.Name)
		}
		if slices.ContainsFunc(d.Sealed[:i], func(o Sealed) bool { return o.Name == x.Name }) {
			return bad("%q is sealed twice", x.Name)
		}
	}
	for _, n := range sortedKeys(d.Trigger) {
		tr := d.Trigger[n]
		if err := tr.check(); err != nil {
			return err
		}
		if (tr.Group != "") != (len(d.Trigger) > 1) {
			return bad("trigger %q: group goes with several rules and only with several rules", n)
		}
	}
	return nil
}

func (in *Input) check() error {
	if !Name.MatchString(in.Name) {
		return bad("bad input name %q", in.Name)
	}
	if in.Type != "" && !slices.Contains(slotTypes, in.Type) {
		return bad("unknown slot type %q", in.Type)
	}
	if in.Sequence != nil {
		if err := in.Sequence.check("sequence", 0, math.MaxUint32); err != nil {
			return err
		}
	}
	return in.Contract.check()
}

func (s *Schedule) check() error {
	set := 0
	for _, isSet := range []bool{s.Immediate != nil, s.BeforeExpiry != nil, s.MinConfirmations != nil} {
		if isSet {
			set++
		}
	}
	switch {
	case set != 1:
		return bad("a schedule has exactly one field")
	case s.Immediate != nil && !*s.Immediate:
		return bad("immediate must be true")
	case s.BeforeExpiry != nil:
		return s.BeforeExpiry.check("before_expiry_seconds", 1, math.MaxUint32)
	case s.MinConfirmations != nil:
		return integer(*s.MinConfirmations, "min_confirmations", 1, math.MaxInt64)
	}
	return nil
}

func (c *Construction) check() error {
	if c.Taptree != "" && !namedVar.MatchString(c.Taptree) {
		return bad("taptree must name a bytes variable, got %q", c.Taptree)
	}
	return nil
}

func (o *Output) check() error {
	if !Name.MatchString(o.Name) {
		return bad("bad output name %q", o.Name)
	}
	if o.Type != "" && !slices.Contains(slotTypes, o.Type) {
		return bad("unknown slot type %q", o.Type)
	}
	if len(o.Value.Draws) == 0 {
		return bad("output %q draws from no input", o.Name)
	}
	if o.Locking.Contract != nil {
		if err := o.Locking.Contract.check(); err != nil {
			return err
		}
	}
	for _, r := range o.Assets {
		if err := r.check(); err != nil {
			return err
		}
	}
	return nil
}

func (r *AssetRoute) check() error {
	switch {
	case (r.From == "") == (r.Issue == ""):
		return bad("an asset route takes from an input or an issue")
	case r.Issue != "" && (r.Amount == nil || r.Asset != ""):
		return bad("an issue route has an amount and no asset")
	case r.From != "" && r.Amount != nil && r.Asset == "":
		return bad("an asset amount needs an asset")
	}
	return nil
}

func (is *Issue) check() error {
	if !Name.MatchString(is.Name) {
		return bad("bad issue name %q", is.Name)
	}
	if is.Asset != "" && (is.Control != nil || len(is.Metadata) > 0) {
		return bad("issue %q: control and metadata go with a new asset only", is.Name)
	}
	return nil
}

func (p *Packets) check() error {
	if p.OutputIndex != nil {
		if err := integer(*p.OutputIndex, "output_index", 0, math.MaxInt64); err != nil {
			return err
		}
	}
	if p.Unmatched != "" && !slices.Contains(unmatcheds, p.Unmatched) {
		return bad("unmatched must be reject or drop")
	}
	for _, r := range p.Rules {
		if err := integer(r.Type, "rule type", 2, math.MaxUint8); err != nil {
			return err
		}
		if r.Type == 5 {
			return bad("packet type 5 is reserved")
		}
		if !slices.Contains(actions, r.Action) {
			return bad("unknown rule action %q", r.Action)
		}
		if (r.Data != nil) != (r.Action == "emit") {
			return bad("data goes with emit and only with emit")
		}
	}
	for _, a := range p.Advertise {
		if a.Template != "self" && !idHex.MatchString(a.Template) {
			return bad("bad advertisement target %q", a.Template)
		}
		if len(a.Outputs) == 0 {
			return bad("an advertisement needs outputs")
		}
	}
	return nil
}

func (tr *Trigger) check() error {
	if err := integer(tr.Packet, "trigger packet", 0, math.MaxUint8); err != nil {
		return err
	}
	if tr.Decode != "" && !slices.Contains(decodes, tr.Decode) {
		return bad("trigger decode must be tlv, records or raw")
	}
	if err := integer(tr.ExpiresAfter, "expires_after_seconds", 1, math.MaxUint32); err != nil {
		return err
	}
	if err := integer(tr.MinValue, "trigger min_value", 0, math.MaxInt64); err != nil {
		return err
	}
	if len(tr.Require) == 0 {
		return bad("a trigger rule requires at least one field")
	}
	if len(tr.Capture) == 0 {
		return bad("a trigger rule captures at least one variable")
	}
	return nil
}

func integer(n int64, what string, lo, hi int64) error {
	if n < lo || n > hi {
		return bad("%s must be an integer in %d..%d, got %d", what, lo, hi, n)
	}
	return nil
}

func bad(format string, args ...any) error {
	return fmt.Errorf("%w: %s", failure.ErrInvalidTemplate, fmt.Sprintf(format, args...))
}
