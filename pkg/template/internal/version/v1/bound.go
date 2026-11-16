package v1

import (
	"bytes"
	"fmt"

	"github.com/arkade-os/executor/pkg/template/internal/model"
	"github.com/btcsuite/btcd/btcec/v2"
)

// BoundTemplate never holds state derived from a particular source or plaintext.
type BoundTemplate struct {
	tmpl *Template
	keys Keys
	vars map[string][]byte
}

func (t *Template) Bind(b model.Bindings) (*BoundTemplate, error) {
	if err := t.checkKeys(b.Keys); err != nil {
		return nil, err
	}
	if err := t.validateVariables(b.Variables, true); err != nil {
		return nil, err
	}
	vars := make(map[string][]byte, len(b.Variables))
	for n, v := range b.Variables {
		vars[n] = bytes.Clone(v)
	}
	clone := func(k *btcec.PublicKey) *btcec.PublicKey {
		if k == nil {
			return nil
		}
		copy := *k
		return &copy
	}
	return &BoundTemplate{
		tmpl: t,
		vars: vars,
		keys: Keys{
			Server: clone(b.Keys.Server), Emulator: clone(b.Keys.Emulator),
			Cosigner: clone(b.Keys.Cosigner),
		},
	}, nil
}
func (b *BoundTemplate) Build(r model.BuildRequest) (*Action, error) {
	if r.Sources == nil {
		return nil, fmt.Errorf("%w: no sources", ErrIneligible)
	}
	i, err := b.tmpl.Instantiate(
		Context{Keys: b.keys, Variables: b.vars, Decrypt: r.Runtime.Decrypt, Sources: r.Sources,
			Coins: r.Coins},
	)
	if err != nil {
		return nil, err
	}
	return i.Build(r.Sources, r.Fee)
}
func (b *BoundTemplate) InputContracts(r model.Runtime) (*Instance, error) {
	return b.tmpl.Instantiate(Context{Keys: b.keys, Variables: b.vars, Decrypt: r.Decrypt})
}
