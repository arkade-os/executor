package delegation

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/arkade-os/executor/pkg/template"
	"github.com/btcsuite/btcd/address/v2"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/chaincfg/v2"
	"github.com/btcsuite/btcd/txscript/v2"
)

const (
	StatusActive    = "active"
	StatusCancelled = "cancelled"
	StatusExpired   = "expired"

	maxDelegationInputs = 64
)

// Input is a template input: the daemon watches its script for coins and runs the template on them.
type Input struct {
	Name       string
	Onchain    bool
	Script     string                  // hex pkScript
	Tapscripts []string                // hex leaves
	Locktime   arklib.AbsoluteLocktime // of the spend leaf, zero means no locktime
	Address    string
	Sequence   uint32
	TxLocktime uint32
}

type Delegation struct {
	ID          int64
	Fingerprint string
	StreamID    int64 // the stream that spawned it, 0 for a client
	TemplateID  string
	Variables   map[string]string // lower-case hex
	Inputs      []Input
	ExpiresAt   *time.Time
	Status      string
	CreatedAt   time.Time
	UpdatedAt   time.Time
}

// ListFilter selects delegations: an empty Status means any, a non-zero Cursor the ids strictly
// below it.
type ListFilter struct {
	Status string
	Cursor int64
	Limit  int
}

type DelegationRepository interface {
	// AddOrUpdate inserts d when its id is zero, filling the id and times in, else persists its
	// status; ErrDelegationNotFound when no row has its id.
	AddOrUpdate(ctx context.Context, d *Delegation) error
	Get(ctx context.Context, id int64) (*Delegation, error)
	// GetByAddress returns the newest delegation watching address, an active one first.
	GetByAddress(ctx context.Context, address string) (*Delegation, error)
	// GetByFingerprint returns the newest delegation with the fingerprint.
	GetByFingerprint(ctx context.Context, fingerprint string) (*Delegation, error)
	// Expire marks the active delegations whose expiry is not after before as expired and returns
	// them.
	Expire(ctx context.Context, before time.Time) ([]Delegation, error)
	// ExpireIdle marks as expired at most limit of the active delegations clients registered
	// before the time that never held a coin, and returns them.
	ExpireIdle(ctx context.Context, registeredBefore time.Time, limit int) ([]Delegation, error)
	// List returns the delegations matching f, newest first.
	List(ctx context.Context, f ListFilter) ([]Delegation, error)
	// CountActive counts the active delegations clients registered and those streams spawned.
	CountActive(ctx context.Context) (clients, spawned int64, err error)
	// Prune deletes the delegations a stream spawned that expired before the time, unless a
	// successor names one as its parent.
	Prune(ctx context.Context, before time.Time) (int64, error)
	// PruneIdle deletes the expired delegations clients registered that never held a coin nor
	// made an attempt, and expired before the time.
	PruneIdle(ctx context.Context, before time.Time) (int64, error)
}

type DelegationParams struct {
	Template  *template.Template
	Variables map[string]string
	ExpiresAt *time.Time
	Now       time.Time
	MinExpiry time.Duration // zero only refuses the past
	Keys      template.Keys
	Decrypt   func([]byte) ([]byte, error) // nil without an encryption key
	Network   arklib.Network
}

func NewDelegation(p DelegationParams) (*Delegation, error) {
	if err := p.check(); err != nil {
		return nil, err
	}
	if len(p.Template.Triggers()) > 0 {
		return nil, ErrStreamOnly
	}
	if p.ExpiresAt != nil &&
		(!p.ExpiresAt.After(p.Now) || p.ExpiresAt.Before(p.Now.Add(p.MinExpiry))) {
		return nil, fmt.Errorf(
			"%w: expires_at must be at least %s ahead", ErrInvalidArgs, p.MinExpiry,
		)
	}
	return p.build(0)
}

func (d *Delegation) Cancel() error {
	if d.Status != StatusActive {
		return ErrNotActive
	}
	d.Status = StatusCancelled
	return nil
}

func (d *Delegation) Resume() error {
	if d.Status != StatusCancelled {
		return ErrNotCancelled
	}
	d.Status = StatusActive
	return nil
}

func (d Delegation) Instantiate(
	tmpl *template.Template, keys template.Keys, decrypt func([]byte) ([]byte, error),
) (*template.Instance, error) {
	return instantiate(tmpl, d.Variables, keys, decrypt)
}

func (p DelegationParams) check() error {
	slots := p.Template.Inputs()
	if len(slots) == 0 || len(slots) > maxDelegationInputs {
		return fmt.Errorf(
			"%w: a delegation has 1 to %d inputs", ErrInvalidArgs, maxDelegationInputs,
		)
	}
	for _, slot := range slots {
		if slot.AllOutputs {
			return fmt.Errorf("%w: all_outputs is not served", ErrInvalidArgs)
		}
	}
	return nil
}

func (p DelegationParams) build(streamID int64) (*Delegation, error) {
	if len(p.Template.Sealed()) > 0 && p.Decrypt == nil {
		return nil, ErrNoEncryptionKey
	}
	inst, err := instantiate(p.Template, p.Variables, p.Keys, p.Decrypt)
	if err != nil {
		return nil, err
	}
	inputs := make([]Input, 0, len(p.Template.Inputs()))
	seen := map[string]struct{}{}
	for i, slot := range p.Template.Inputs() {
		locktime, _ := inst.Locktime(i)
		in := Input{
			Name: slot.Name, Onchain: slot.Onchain, Script: hex.EncodeToString(inst.PkScript(i)),
			Tapscripts: inst.Tapscripts(i), Locktime: locktime,
			Sequence: inst.Sequence(i), TxLocktime: inst.TxLocktime(),
		}
		if err := checkCosignerKeyLeaf(in.Tapscripts, p.Keys.Cosigner); err != nil {
			return nil, err
		}
		in.Address, err = addressOf(inst.PkScript(i), in.Onchain, p.Keys.Server, p.Network)
		if err != nil {
			return nil, err
		}
		key := in.Script + ":0"
		if in.Onchain {
			key = in.Script + ":1"
		}
		if _, ok := seen[key]; ok {
			return nil, fmt.Errorf("%w: two inputs watch the same script", ErrInvalidArgs)
		}
		seen[key] = struct{}{}
		inputs = append(inputs, in)
	}
	if err := inst.OfflineSpend(p.Template.Type() == template.Onchain); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
	}
	if p.Variables == nil {
		p.Variables = map[string]string{}
	}
	return &Delegation{
		Fingerprint: fingerprint(p.Template.ID(), p.Variables), TemplateID: p.Template.ID(),
		Variables: p.Variables, Inputs: inputs, StreamID: streamID, ExpiresAt: p.ExpiresAt,
		Status: StatusActive,
	}, nil
}

func instantiate(
	tmpl *template.Template, variables map[string]string, keys template.Keys,
	decrypt func([]byte) ([]byte, error),
) (*template.Instance, error) {
	decoded, err := decodeVariables(variables)
	if err != nil {
		return nil, err
	}
	inst, err := tmpl.Instantiate(template.Context{
		Keys: keys, Variables: decoded, Decrypt: decrypt,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArgs, err)
	}
	return inst, nil
}

// fingerprint is hex(sha256(templateID || 0 || variables as JSON with sorted keys)).
func fingerprint(templateID string, variables map[string]string) string {
	if variables == nil {
		variables = map[string]string{}
	}
	vars, _ := json.Marshal(variables) // a map of strings always marshals, keys sorted
	h := sha256.New()
	h.Write([]byte(templateID))
	h.Write([]byte{0})
	h.Write(vars)
	return hex.EncodeToString(h.Sum(nil))
}

func decodeVariables(variables map[string]string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(variables))
	for name, value := range variables {
		raw, err := hex.DecodeString(value)
		if err != nil || hex.EncodeToString(raw) != value {
			return nil, fmt.Errorf("%w: variable %q is not lower-case hex", ErrInvalidArgs, name)
		}
		out[name] = raw
	}
	return out, nil
}

// checkCosignerKeyLeaf refuses tapscripts pushing the cosigner's x-only key.
func checkCosignerKeyLeaf(tapscripts []string, cosigner *btcec.PublicKey) error {
	key := schnorr.SerializePubKey(cosigner)
	for _, leaf := range tapscripts {
		script, err := hex.DecodeString(leaf)
		if err != nil {
			return err
		}
		holds, err := pushes(script, key)
		if err != nil {
			return err
		}
		if holds {
			return ErrCosignerKeyLeaf
		}
	}
	return nil
}

// pushes reports whether script pushes data exactly.
func pushes(script, data []byte) (bool, error) {
	tok := txscript.MakeScriptTokenizer(0, script)
	for tok.Next() {
		if bytes.Equal(tok.Data(), data) {
			return true, nil
		}
	}
	return false, tok.Err()
}

func addressOf(
	pkScript []byte, onchain bool, server *btcec.PublicKey, network arklib.Network,
) (string, error) {
	if len(pkScript) != 34 || pkScript[0] != txscript.OP_1 || pkScript[1] != txscript.OP_DATA_32 {
		return "", fmt.Errorf("%w: input script is not taproot", ErrInvalidArgs)
	}
	if !onchain {
		key, err := schnorr.ParsePubKey(pkScript[2:])
		if err != nil {
			return "", err
		}
		return (&arklib.Address{HRP: network.Addr, Signer: server, VtxoTapKey: key}).EncodeV0()
	}
	params, err := chainParams(network)
	if err != nil {
		return "", err
	}
	addr, err := address.NewAddressTaproot(pkScript[2:], params)
	if err != nil {
		return "", err
	}
	return addr.EncodeAddress(), nil
}

func chainParams(network arklib.Network) (*chaincfg.Params, error) {
	switch network.Name {
	case arklib.Bitcoin.Name:
		return &chaincfg.MainNetParams, nil
	case arklib.BitcoinTestNet.Name:
		return &chaincfg.TestNet3Params, nil
	case arklib.BitcoinTestNet4.Name:
		return &chaincfg.TestNet4Params, nil
	case arklib.BitcoinSigNet.Name:
		return &chaincfg.SigNetParams, nil
	case arklib.BitcoinMutinyNet.Name:
		return &arklib.MutinyNetSigNetParams, nil
	case arklib.BitcoinRegTest.Name:
		return &chaincfg.RegressionNetParams, nil
	}
	return nil, fmt.Errorf("unknown network %q", network.Name)
}
