package execution

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/arkade-os/emulator/pkg/arkade"
	delegationapp "github.com/arkade-os/executor/internal/core/application/delegation"
	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/arkade-os/executor/pkg/template"
	"github.com/arkade-os/executor/pkg/template/artifact"
	"github.com/arkade-os/executor/templates"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/wire/v2"
	"github.com/stretchr/testify/require"
)

func TestTrack(t *testing.T) {
	for _, busy := range []bool{false, true} {
		name := "idle"
		if busy {
			name = "busy"
		}
		t.Run(name, func(t *testing.T) {
			s := &Service{pending: map[int64]*pending{}}
			a := delegation.Attempt{ID: 1, Coins: []string{"coin:0"}, Status: delegation.Submitted}
			p := s.track(a)
			require.Same(t, p, s.pending[a.ID])
			require.Equal(t, a, p.attempt)
			require.True(t, p.busy)
			require.NotNil(t, p.spent)
			require.Empty(t, p.spent)
			require.Zero(t, p.landBy)

			spent := p.spent
			spent["coin:0"] = "commitment"
			landBy := time.Unix(1_800_000_000, 0)
			p.busy, p.landBy = busy, landBy
			a.AddTx(delegation.Tx{Txid: "commitment", Onchain: true, Spends: true})

			updated := s.track(a)
			require.Same(t, p, updated)
			require.Equal(t, a, updated.attempt)
			require.Equal(t, busy, updated.busy)
			require.Equal(t, landBy, updated.landBy)
			require.Equal(t, map[string]string{"coin:0": "commitment"}, updated.spent)
			spent["coin:1"] = "commitment"
			require.Equal(t, "commitment", updated.spent["coin:1"])
			require.Len(t, s.pending, 1)
		})
	}
}

func TestDueAt(t *testing.T) {
	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	inst := instance(t, templates.Templates["renew.json"], map[string]string{
		"owner": hex.EncodeToString(
			owner.PubKey().SerializeCompressed(),
		),
		"exit_delay":     scriptNum(1<<22 | 1),
		"renewal_window": scriptNum(1024),
		"max_fee":        scriptNum(1000),
	})
	c := delegation.Coin{
		Outpoint: strings.Repeat("ab", 32) + ":1", Amount: 1000, CreatedAt: now,
		ExpiresAt: now.Add(time.Hour),
	}
	require.Equal(t, c.ExpiresAt.Add(-1024*time.Second), dueAt(inst, c, now))

	// a short life halves the lead
	c.ExpiresAt = now.Add(10 * time.Minute)
	require.Equal(t, now.Add(5*time.Minute), dueAt(inst, c, now))

	// without an expiry the schedule cannot be placed: an hour from now
	c.ExpiresAt = time.Time{}
	require.Equal(t, now.Add(time.Hour), dueAt(inst, c, now))

	// no schedule: due at creation, or now when unknown
	plain := instance(t, fixture(t, "minimal.json"), nil)
	c = delegation.Coin{Outpoint: "bad outpoint", Amount: 1, CreatedAt: now.Add(-time.Minute)}
	require.Equal(t, c.CreatedAt, dueAt(plain, c, now))
	c.CreatedAt = time.Time{}
	require.Equal(t, now, dueAt(plain, c, now))
}

func TestDueAtOnchainWaitsForConfirmations(t *testing.T) {
	owner, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	inst := instance(t, templates.Templates["board.json"], map[string]string{
		"owner": hex.EncodeToString(
			owner.PubKey().SerializeCompressed(),
		),
		"boarding_exit_delay": scriptNum(1<<22 | 2),
		"exit_delay": scriptNum(
			1<<22 | 1,
		),
		"renewal_window": scriptNum(1024),
		"max_fee":        scriptNum(1000),
	})
	c := delegation.Coin{
		Outpoint: strings.Repeat("ab", 32) + ":0", Amount: 1000, CreatedAt: now.Add(-time.Minute),
		Onchain: true,
	}
	require.Equal(t, now.Add(time.Hour), dueAt(inst, c, now), "unconfirmed")
	require.True(t, early(inst, []delegation.Coin{c}, now), "its hour over, it still waits")
	c.Confirmations = 1
	require.Equal(t, c.CreatedAt, dueAt(inst, c, now))
	require.False(t, early(inst, []delegation.Coin{c}, now))
}

// instance instantiates doc with variables, hex-encoded, under fresh keys.
func TestAttributeFollowsTheSpenderTransaction(t *testing.T) {
	spent := wire.OutPoint{Hash: [32]byte{1}}
	missed := wire.OutPoint{Hash: [32]byte{2}}
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{PreviousOutPoint: spent})
	txid := tx.TxHash().String()
	attempt := delegation.Attempt{
		ID: 1, Coins: []string{spent.String(), missed.String()}, Status: delegation.Submitted,
		Txs: []delegation.Tx{{Txid: txid, Onchain: true, Spends: true}},
	}
	s := &Service{ctx: t.Context(), chain: chainTx{tx: tx}}
	filled := &pending{
		attempt: attempt, spent: map[string]string{spent.String(): "", missed.String(): txid},
	}
	require.Empty(t, s.attribute(filled))
	require.Equal(t, txid, filled.spent[spent.String()])

	conflict := &pending{attempt: attempt, spent: map[string]string{missed.String(): ""}}
	require.Equal(t, missed.String(), s.attribute(conflict))

	s.chain = chainTx{err: errors.New("not found")}
	unknown := &pending{attempt: attempt, spent: map[string]string{spent.String(): ""}}
	require.Empty(t, s.attribute(unknown))
	require.Empty(t, unknown.spent[spent.String()])

	known := &pending{attempt: attempt, spent: map[string]string{spent.String(): txid}}
	require.Empty(t, s.attribute(known))
}

func TestSettleLeavesADisappearanceSubmitted(t *testing.T) {
	op := wire.OutPoint{Hash: [32]byte{9}}.String()
	p := &pending{
		attempt: delegation.Attempt{
			ID: 7, Coins: []string{op}, Status: delegation.Submitted,
			Txs: []delegation.Tx{{Txid: "ark", Spends: true}},
		},
		spent: map[string]string{op: ""},
	}
	s := &Service{
		ctx: t.Context(), pending: map[int64]*pending{p.attempt.ID: p},
		chain: chainTx{err: errors.New("not found")},
	}
	s.settle(p, time.Unix(1_700_000_000, 0))
	require.Equal(t, delegation.Submitted, p.attempt.Status)
	require.Contains(t, s.pending, p.attempt.ID)
}

func TestSettleWaitsUntilTheBatchNamesItsSpender(t *testing.T) {
	op := wire.OutPoint{Hash: [32]byte{4}}.String()
	now := time.Unix(1_700_000_000, 0)
	p := &pending{
		attempt: delegation.Attempt{
			ID: 4, Coins: []string{op}, Status: delegation.Submitted,
			Txs: []delegation.Tx{{Txid: "proof"}},
		},
		busy: true, spent: map[string]string{op: "commitment"},
	}
	saved := &storeAttempts{}
	s := &Service{
		ctx: t.Context(), pending: map[int64]*pending{p.attempt.ID: p},
		attempts: saved, coins: releaseCoins{},
	}
	s.settle(p, now)
	require.Equal(t, delegation.Submitted, p.attempt.Status)
	require.Contains(t, s.pending, p.attempt.ID)
	require.Nil(t, saved.attempt)

	p.attempt.Txs = []delegation.Tx{{Txid: "ark", Spends: true}}
	p.spent[op] = "commitment"
	s.settle(p, now)
	require.NotContains(t, s.pending, p.attempt.ID)
	require.Equal(t, delegation.Failed, saved.attempt.Status)
	require.Contains(t, saved.attempt.Error, "spent by commitment")
}

func TestSettleAdoptsOneObservedSpender(t *testing.T) {
	first := wire.OutPoint{Hash: [32]byte{5}}.String()
	second := wire.OutPoint{Hash: [32]byte{6}}.String()
	now := time.Unix(1_700_000_000, 0)
	p := &pending{
		attempt: delegation.Attempt{
			ID: 5, Coins: []string{first, second}, Status: delegation.Submitted,
			Txs: []delegation.Tx{{Txid: "proof"}},
		},
		spent: map[string]string{first: "commitment", second: "commitment"},
	}
	saved := &storeAttempts{}
	var released []string
	s := &Service{
		ctx: t.Context(), pending: map[int64]*pending{p.attempt.ID: p},
		attempts: saved, coins: releasedCoins{outpoints: &released},
		delegations: delegationService(t),
	}
	s.settle(p, now)
	require.NotContains(t, s.pending, p.attempt.ID)
	require.Equal(t, delegation.Succeeded, saved.attempt.Status)
	require.Equal(t, []string{"commitment"}, saved.attempt.Spenders())
	require.Equal(t, []string{first, second}, released, "a succeeded attempt holds no coin")

	other := &pending{
		attempt: delegation.Attempt{
			ID: 6, Coins: []string{first, second}, Status: delegation.Submitted,
			Txs: []delegation.Tx{{Txid: "proof"}},
		},
		spent: map[string]string{first: "commitment", second: "other"},
	}
	s.pending[other.attempt.ID] = other
	s.settle(other, now)
	require.NotContains(t, s.pending, other.attempt.ID)
	require.Equal(t, delegation.Failed, saved.attempt.Status)
	require.Contains(t, saved.attempt.Error, "different transactions")
}

func TestSettleFindsAnOnchainSpender(t *testing.T) {
	op := wire.OutPoint{Hash: [32]byte{7}}.String()
	now := time.Unix(1_700_000_000, 0)
	p := &pending{
		attempt: delegation.Attempt{
			ID: 8, DelegationID: 3, Coins: []string{op}, Status: delegation.Submitted,
			Txs: []delegation.Tx{{Txid: "proof"}},
		},
		spent: map[string]string{op: ""},
	}
	saved := &storeAttempts{}
	s := &Service{
		ctx: t.Context(), pending: map[int64]*pending{p.attempt.ID: p},
		attempts: saved, delegations: delegationService(t),
		coins: listedCoins{coins: []delegation.Coin{{
			Outpoint: op, Script: "aa", Onchain: true,
		}}},
		chain: spenderChain{txid: "commitment"},
	}
	s.settle(p, now)
	require.NotContains(t, s.pending, p.attempt.ID)
	require.Equal(t, delegation.Succeeded, saved.attempt.Status)
	require.Equal(t, []string{"commitment"}, saved.attempt.Spenders())
}

func delegationService(t *testing.T) *delegationapp.Service {
	t.Helper()
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := key.PubKey()
	return delegationapp.NewService(
		nil, missingDelegation{}, nil, nil, pub, nil, pub, pub, arklib.BitcoinRegTest,
		delegationapp.Limits{},
	)
}

func TestRebroadcastSendsOnlyAWitnessTransaction(t *testing.T) {
	tx := wire.NewMsgTx(2)
	tx.AddTxIn(&wire.TxIn{Witness: wire.TxWitness{[]byte{1}}})
	var signed bytes.Buffer
	require.NoError(t, tx.Serialize(&signed))
	plain := wire.NewMsgTx(2)
	plain.AddTxIn(&wire.TxIn{})
	var unsigned bytes.Buffer
	require.NoError(t, plain.Serialize(&unsigned))
	chain := &recordChain{}
	s := &Service{ctx: t.Context(), chain: chain}
	s.rebroadcast(delegation.Attempt{Txs: []delegation.Tx{
		{Onchain: true, Spends: true, Raw: signed.Bytes(), Txid: tx.TxHash().String()},
		{Onchain: true, Spends: true, Raw: unsigned.Bytes(), Txid: plain.TxHash().String()},
		{Onchain: false, Spends: true, Raw: signed.Bytes(), Txid: "ark"},
		{Raw: []byte("psbt")},
	}})
	require.Equal(t, []string{tx.TxHash().String()}, chain.ids)
}

func instance(t *testing.T, doc []byte, variables map[string]string) *template.Instance {
	t.Helper()
	docs := map[string][]byte{}
	for _, a := range templates.Artifacts {
		if parsed, err := artifact.Parse(a); err == nil {
			docs[parsed.ID()] = a
		}
	}
	tmpl, err := template.Parse(doc, docs)
	require.NoError(t, err)
	decoded := map[string][]byte{}
	for name, value := range variables {
		raw, err := hex.DecodeString(value)
		require.NoError(t, err)
		decoded[name] = raw
	}
	keys := make([]*btcec.PublicKey, 3)
	for i := range keys {
		k, err := btcec.NewPrivateKey()
		require.NoError(t, err)
		keys[i] = k.PubKey()
	}
	inst, err := tmpl.Instantiate(template.Context{
		Keys:      template.Keys{Operator: keys[0], Emulator: keys[1], BatchCosigner: keys[2]},
		Variables: decoded,
	})
	require.NoError(t, err)
	return inst
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	doc, err := os.ReadFile("../../../../pkg/template/testdata/" + name)
	require.NoError(t, err)
	return doc
}

func scriptNum(n int64) string {
	b, _ := arkade.BigNumFromInt64(n).Bytes()
	return hex.EncodeToString(b)
}

type chainTx struct {
	tx  *wire.MsgTx
	err error
}

func (chainTx) Subscribe(context.Context, []string) (ports.ChainSubscription, error) {
	return nil, errors.New("unused")
}

func (chainTx) Unspents(context.Context, string) ([]ports.Output, error) {
	return nil, errors.New("unused")
}

func (chainTx) Tip(context.Context) (delegation.Tip, error) {
	return delegation.Tip{}, errors.New("unused")
}

func (chainTx) MedianTime(context.Context, int64) (time.Time, error) {
	return time.Time{}, errors.New("unused")
}

func (c chainTx) Transaction(context.Context, string) (*wire.MsgTx, error) { return c.tx, c.err }

func (chainTx) Spender(context.Context, string, string) (string, error) {
	return "", errors.New("unused")
}

func (chainTx) Broadcast(context.Context, *wire.MsgTx) (string, error) {
	return "", errors.New("unused")
}

func (chainTx) FeeRate(context.Context) (float64, error) { return 0, errors.New("unused") }

type recordChain struct {
	chainTx
	ids []string
}

func (c *recordChain) Broadcast(_ context.Context, tx *wire.MsgTx) (string, error) {
	id := tx.TxHash().String()
	c.ids = append(c.ids, id)
	return id, nil
}

type storeAttempts struct{ attempt *delegation.Attempt }

func (s *storeAttempts) AddOrUpdate(_ context.Context, a *delegation.Attempt) error {
	cp := *a
	s.attempt = &cp
	return nil
}

func (storeAttempts) ListPending(context.Context) ([]delegation.Attempt, error) {
	return nil, nil
}

func (storeAttempts) ListByDelegation(
	context.Context, int64, int64, int,
) ([]delegation.Attempt, error) {
	return nil, nil
}

type missingDelegation struct {
	delegation.DelegationRepository
}

func (missingDelegation) Get(context.Context, int64) (*delegation.Delegation, error) {
	return nil, delegation.ErrDelegationNotFound
}

type releaseCoins struct{ delegation.CoinRepository }

type releasedCoins struct {
	delegation.CoinRepository
	outpoints *[]string
}

type listedCoins struct {
	releaseCoins
	coins []delegation.Coin
}

func (l listedCoins) ListByDelegation(context.Context, int64) ([]delegation.Coin, error) {
	return l.coins, nil
}

type spenderChain struct {
	chainTx
	txid string
}

func (c spenderChain) Spender(context.Context, string, string) (string, error) {
	return c.txid, nil
}

func (releaseCoins) Release(context.Context, []string, time.Time) error { return nil }

func (r releasedCoins) Release(_ context.Context, outpoints []string, _ time.Time) error {
	*r.outpoints = append(*r.outpoints, outpoints...)
	return nil
}
