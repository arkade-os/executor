package v1

import (
	"encoding/hex"
	"fmt"
	"maps"
	"math/bits"
	"slices"
	"strconv"

	"github.com/arkade-os/arkd/pkg/ark-lib/asset"
	"github.com/arkade-os/arkd/pkg/ark-lib/extension"
	"github.com/arkade-os/executor/pkg/template/internal/version/v1/schema"
)

type assetRoute struct {
	from   int          // -1 for an issue route
	issue  int          // -1 unless an issue route
	spec   string       // the asset as written, "" for catch-all
	asset  byteTemplate // resolves to a 34-byte asset id, nil for catch-all
	amount byteTemplate // nil for the remainder
}

type issue struct {
	name     string
	asset    byteTemplate // nil for a new asset
	control  *control
	metadata []metadata
}

type control struct {
	asset byteTemplate
	issue int // -1 unless the control is another issue
}

type metadata struct {
	key   string
	value byteTemplate
}

func (c *compiler) issues() error {
	names := map[string]int{}
	for k, s := range c.doc.Issues {
		if _, ok := names[s.Name]; ok {
			return bad("duplicate issue %q", s.Name)
		}
		names[s.Name] = k
	}
	for _, s := range c.doc.Issues {
		is := issue{name: s.Name}
		var err error
		if s.Asset != "" {
			if is.asset, err = c.bind(schema.Binding{Str: &s.Asset}, "asset"); err != nil {
				return err
			}
		}
		if ct := s.Control; ct != nil {
			is.control = &control{issue: -1}
			if ct.Issue != "" {
				k, ok := names[ct.Issue]
				if !ok || ct.Issue == s.Name || c.doc.Issues[k].Asset != "" {
					return bad("issue %q: control %q names no other new asset", s.Name, ct.Issue)
				}
				is.control.issue = k
			} else if is.control.asset, err = c.bind(
				schema.Binding{Str: &ct.Asset}, "asset",
			); err != nil {
				return err
			}
		}
		for _, key := range slices.Sorted(maps.Keys(s.Metadata)) {
			v := s.Metadata[key]
			b, err := c.bind(schema.Binding{Str: &v}, "")
			if err != nil {
				return err
			}
			is.metadata = append(is.metadata, metadata{key: key, value: b})
		}
		c.t.issues = append(c.t.issues, is)
	}
	return nil
}

func (c *compiler) route(s *schema.AssetRoute) (assetRoute, error) {
	r := assetRoute{from: -1, issue: -1}
	var err error
	if s.Issue != "" {
		r.issue = slices.IndexFunc(c.t.issues, func(is issue) bool { return is.name == s.Issue })
		if r.issue < 0 {
			return r, bad("asset route names no issue %q", s.Issue)
		}
		r.amount, err = c.amount(*s.Amount)
		return r, err
	}
	if r.from, err = c.inputIndex(s.From, "asset route"); err != nil {
		return r, err
	}
	if c.t.inputs[r.from].onchain {
		return r, bad("onchain input %q carries no assets", c.t.inputs[r.from].name)
	}
	if s.Asset != "" {
		r.spec = s.Asset
		if r.asset, err = c.bind(schema.Binding{Str: &s.Asset}, "asset"); err != nil {
			return r, err
		}
	}
	if s.Amount != nil {
		r.amount, err = c.amount(*s.Amount)
	}
	return r, err
}

// sameAsset compares assets as written; two spellings of one id are caught when routed.
func (r assetRoute) sameAsset(x assetRoute) bool {
	return r.from == x.from && r.issue == x.issue && r.spec == x.spec
}

// sink is an output or a burn; ctx is the slot of its expressions.
type sink struct {
	name   string
	copy   bool // a copy of an each output
	ctx    int
	routes []assetRoute
}

// sinks lists the outputs, then the burns; allocation.assets is indexed alike.
func (t *Template) sinks() []sink {
	out := make([]sink, 0, len(t.outputs)+len(t.burns))
	for _, o := range t.outputs {
		out = append(out, sink{
			name: "output " + strconv.Quote(
				o.name,
			),
			copy:   o.coin >= 0,
			ctx:    o.from,
			routes: o.assets,
		})
	}
	for _, b := range t.burns {
		out = append(out, sink{name: "a burn", ctx: b.from, routes: []assetRoute{b}})
	}
	return out
}

// routeAssets credits fixed amounts, then named remainders, then the catch-all; what stays in held
// has no route.
func (i *Instance) routeAssets(
	d *draft, slot int, held map[string]uint64, a *allocation, copies bool,
) error {
	named := map[string]bool{}
	sinks := i.tmpl.sinks()
	for pass := range 3 {
		for k, o := range sinks {
			for _, r := range o.routes {
				if r.from != slot || o.copy != copies || pass != routePass(r) {
					continue
				}
				var key string
				if r.asset != nil {
					id, err := i.decoded(r.asset, "asset", d, o.ctx)
					if err != nil {
						return fmt.Errorf("%s asset: %w", o.name, err)
					}
					key = hex.EncodeToString(id)
				}
				var err error
				switch pass {
				case 0:
					var n uint64
					if n, err = i.fixedAmount(r.amount, d, o.ctx); err != nil {
						return fmt.Errorf("%s asset %s: %w", o.name, key, err)
					}
					if n > held[key] {
						return fmt.Errorf(
							"%w: %s draws %d of asset %s, input %q holds %d", ErrIneligible,
							o.name, n, key, i.tmpl.inputs[slot].name, held[key],
						)
					}
					held[key] -= n
					err = credit(a.assets[k], key, n)
				case 1:
					if named[key] {
						return fmt.Errorf(
							"%w: input %q has two remainder routes for asset %s", ErrIneligible,
							i.tmpl.inputs[slot].name, key,
						)
					}
					named[key] = true
					err = credit(a.assets[k], key, held[key])
					delete(held, key)
				case 2:
					for key, n := range held {
						if err = credit(a.assets[k], key, n); err != nil {
							break
						}
					}
					clear(held)
				}
				if err != nil {
					return fmt.Errorf("%s: %w", o.name, err)
				}
			}
		}
	}
	return nil
}

// mint credits issue routes: more of an existing asset joins that asset, a new one is counted
// apart since its id is the transaction's.
func (i *Instance) mint(d *draft, a *allocation) error {
	t := i.tmpl
	a.minted = make([][]uint64, len(t.issues))
	for j := range a.minted {
		a.minted[j] = make([]uint64, len(t.outputs))
	}
	for k, o := range t.outputs {
		for _, r := range o.assets {
			if r.issue < 0 {
				continue
			}
			is := t.issues[r.issue]
			n, err := i.fixedAmount(r.amount, d, o.from)
			if err != nil {
				return fmt.Errorf("output %q issue %q: %w", o.name, is.name, err)
			}
			if is.asset == nil {
				sum, carry := bits.Add64(a.minted[r.issue][k], n, 0)
				if carry != 0 {
					return fmt.Errorf("%w: issue %q overflows", ErrIneligible, is.name)
				}
				a.minted[r.issue][k] = sum
				continue
			}
			id, err := i.decoded(is.asset, "asset", d, o.from)
			if err != nil {
				return fmt.Errorf("issue %q asset: %w", is.name, err)
			}
			if err := credit(a.assets[k], hex.EncodeToString(id), n); err != nil {
				return fmt.Errorf("output %q: %w", o.name, err)
			}
		}
	}
	return nil
}

func routePass(r assetRoute) int {
	switch {
	case r.amount != nil:
		return 0
	case r.asset != nil:
		return 1
	}
	return 2
}

// assetPacket is nil when no asset moves. New issues take the first groups in the order listed,
// then existing assets by id.
func assetPacket(
	i *Instance,
	d *draft,
	sources []*Source,
	a *allocation,
) (extension.Packet, error) {
	t := i.tmpl
	ids := map[string]bool{}
	for _, m := range a.assets[:len(t.outputs)] {
		for id := range m {
			ids[id] = true
		}
	}
	for _, s := range sources {
		for _, as := range s.Assets {
			ids[hex.EncodeToString(as.ID)] = true
		}
	}
	var groups []asset.AssetGroup
	newPos := map[int]uint16{}
	for j, is := range t.issues {
		if is.asset == nil {
			newPos[j] = uint16(len(newPos))
		}
	}
	for j, is := range t.issues {
		if is.asset != nil {
			continue
		}
		g, err := i.issueGroup(d, a, j, newPos)
		if err != nil {
			return nil, fmt.Errorf("%w: issue %q: %v", ErrIneligible, is.name, err)
		}
		groups = append(groups, *g)
	}
	for _, id := range slices.Sorted(maps.Keys(ids)) {
		g, err := assetGroup(t, sources, a, id)
		if err != nil {
			return nil, fmt.Errorf("%w: asset %s: %v", ErrIneligible, id, err)
		}
		groups = append(groups, *g)
	}
	if len(groups) == 0 {
		return nil, nil
	}
	p, err := asset.NewPacket(groups)
	if err != nil {
		return nil, fmt.Errorf("%w: assets packet: %v", ErrIneligible, err)
	}
	return p, nil
}

func (i *Instance) issueGroup(
	d *draft, a *allocation, j int, newPos map[int]uint16,
) (*asset.AssetGroup, error) {
	t := i.tmpl
	is := t.issues[j]
	var outs []asset.AssetOutput
	for k, n := range a.minted[j] {
		if n > 0 {
			out, err := asset.NewAssetOutput(t.outputs[k].index, n)
			if err != nil {
				return nil, err
			}
			outs = append(outs, *out)
		}
	}
	var ref *asset.AssetRef
	var err error
	if ct := is.control; ct != nil {
		if ct.issue >= 0 {
			ref, err = asset.NewAssetRefFromGroupIndex(newPos[ct.issue])
		} else {
			var raw []byte
			if raw, err = i.decoded(ct.asset, "asset", d, 0); err != nil {
				return nil, err
			}
			var id *asset.AssetId
			if id, err = asset.NewAssetIdFromBytes(raw); err != nil {
				return nil, err
			}
			ref, err = asset.NewAssetRefFromId(*id)
		}
		if err != nil {
			return nil, err
		}
	}
	var md []asset.Metadata
	for _, m := range is.metadata {
		v, err := i.resolve(m.value, d, d.index(0))
		if err != nil {
			return nil, err
		}
		md = append(md, asset.Metadata{Key: []byte(m.key), Value: v})
	}
	return asset.NewAssetGroup(nil, ref, nil, outs, md)
}

func assetGroup(
	t *Template, sources []*Source, a *allocation, id string,
) (*asset.AssetGroup, error) {
	var ins []asset.AssetInput
	for slot, s := range sources {
		held := uint64(0)
		for _, as := range s.Assets {
			if hex.EncodeToString(as.ID) == id {
				held += as.Amount
			}
		}
		if held == 0 {
			continue
		}
		in, err := asset.NewAssetInput(uint16(slot), held)
		if err != nil {
			return nil, err
		}
		ins = append(ins, *in)
	}
	var outs []asset.AssetOutput
	for k, o := range t.outputs {
		if n := a.assets[k][id]; n > 0 {
			out, err := asset.NewAssetOutput(o.index, n)
			if err != nil {
				return nil, err
			}
			outs = append(outs, *out)
		}
	}
	aid, err := asset.NewAssetIdFromString(id)
	if err != nil {
		return nil, err
	}
	return asset.NewAssetGroup(aid, nil, ins, outs, nil)
}
