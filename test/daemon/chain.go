package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	delegationdomain "github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/wire/v2"
)

// chain stands in for the Electrum server: utxos by script and the transactions that created them,
// a tip whose blocks are mined by the test, a status per script that is the hash of its utxos, and
// counters of what the daemon asked.
type chain struct {
	mu           sync.Mutex
	utxos        map[string][]ports.Output
	txs          map[string]*wire.MsgTx
	times        map[int64]time.Time
	height       int64
	down         bool
	sub          *chainSub
	funded       int
	broadcasts   []*wire.MsgTx
	refuse       error
	unspentCalls map[string]int
}

type chainSub struct {
	c      *chain
	ctx    context.Context
	cancel context.CancelFunc
	events chan ports.ChainEvent

	scripts map[string]struct{} // guarded by c.mu
}

func (c *chain) Fund(script string, amount uint64) string {
	pkScript, err := hex.DecodeString(script)
	if err != nil {
		panic(err)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.funded++
	tx := wire.NewMsgTx(3)
	tx.AddTxIn(
		&wire.TxIn{
			PreviousOutPoint: wire.OutPoint{
				Hash: sha256.Sum256(fmt.Appendf(nil, "funding%d", c.funded)),
			},
		},
	)
	tx.AddTxOut(wire.NewTxOut(int64(amount), pkScript))
	outpoint := tx.TxHash().String() + ":0"
	c.txs[tx.TxHash().String()] = tx
	c.utxos[script] = append(
		c.utxos[script], ports.Output{Outpoint: outpoint, Script: script, Amount: amount},
	)
	c.push(script)
	return outpoint
}

func (c *chain) Mine(n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	first := c.height + 1
	var changed []string
	for script, utxos := range c.utxos {
		for i := range utxos {
			if utxos[i].Height == 0 {
				utxos[i].Height = first
				changed = append(changed, script)
			}
		}
	}
	for range n {
		c.height++
		c.times[c.height] = time.Now()
		c.pushTip()
	}
	slices.Sort(changed)
	for _, script := range changed {
		c.push(script)
	}
}

func (c *chain) SetMedianTime(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for h := c.height - 5; h <= c.height; h++ {
		c.times[h] = at
	}
	c.pushTip()
}

func (c *chain) Drop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = true
}

func (c *chain) Reconnect() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.down = false
	if c.sub != nil {
		c.sub.resync()
	}
}

func (c *chain) UnspentCalls(script string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unspentCalls[script]
}

func (c *chain) ResetCalls() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unspentCalls = map[string]int{}
}

func (c *chain) Subscribe(ctx context.Context, scripts []string) (ports.ChainSubscription, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sub != nil {
		return nil, errors.New("chain already subscribed")
	}
	ctx, cancel := context.WithCancel(ctx)
	c.sub = &chainSub{
		c: c, ctx: ctx, cancel: cancel, events: make(chan ports.ChainEvent, 1024+len(scripts)),
		scripts: map[string]struct{}{},
	}
	for _, script := range scripts {
		c.sub.scripts[script] = struct{}{}
	}
	c.sub.resync()
	return c.sub, nil
}

func (c *chain) Unspents(_ context.Context, script string) ([]ports.Output, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return nil, ports.ErrDisconnected
	}
	c.unspentCalls[script]++
	return slices.Clone(c.utxos[script]), nil
}

func (c *chain) Tip(context.Context) (delegationdomain.Tip, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return delegationdomain.Tip{}, ports.ErrDisconnected
	}
	return c.tip(), nil
}

func (c *chain) MedianTime(_ context.Context, height int64) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return time.Time{}, ports.ErrDisconnected
	}
	at, ok := c.times[height]
	if !ok {
		return time.Time{}, fmt.Errorf("block %d has no median time", height)
	}
	return at, nil
}

func (c *chain) Keep(tx *wire.MsgTx) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.txs[tx.TxHash().String()] = tx.Copy()
}

func (c *chain) Spender(_ context.Context, _, outpoint string) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.down {
		return "", ports.ErrDisconnected
	}
	for id, tx := range c.txs {
		for _, in := range tx.TxIn {
			if in.PreviousOutPoint.String() == outpoint {
				return id, nil
			}
		}
	}
	return "", nil
}

func (c *chain) Transaction(_ context.Context, txid string) (*wire.MsgTx, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	tx, ok := c.txs[txid]
	if !ok {
		return nil, errors.New("transaction not found")
	}
	return tx, nil
}

func (c *chain) Broadcast(_ context.Context, tx *wire.MsgTx) (string, error) {
	c.mu.Lock()
	if c.refuse != nil {
		err := c.refuse
		c.mu.Unlock()
		return "", err
	}
	stored := tx.Copy()
	c.broadcasts = append(c.broadcasts, stored)
	c.txs[stored.TxHash().String()] = stored
	c.mu.Unlock()
	for _, in := range tx.TxIn {
		c.spend(in.PreviousOutPoint.String())
	}
	return stored.TxHash().String(), nil
}

func (c *chain) FeeRate(context.Context) (float64, error) { return 1, nil }

func (c *chain) RefuseBroadcast(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.refuse = err
}

func (c *chain) Broadcasts() []*wire.MsgTx {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.broadcasts)
}

func (s *chainSub) Events() <-chan ports.ChainEvent { return s.events }

func (s *chainSub) Connected() bool {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	return !s.c.down
}

func (s *chainSub) Update(_ context.Context, add, remove []string) error {
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	for _, script := range remove {
		delete(s.scripts, script)
	}
	for _, script := range add {
		s.scripts[script] = struct{}{}
		s.c.push(script)
	}
	return nil
}

func (s *chainSub) Stop() {
	s.cancel()
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if s.c.sub == s {
		s.c.sub = nil
	}
}

func (c *chain) confirmed(outpoint string) (ports.Output, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, utxos := range c.utxos {
		for _, u := range utxos {
			if u.Outpoint == outpoint && u.Height > 0 {
				return u, true
			}
		}
	}
	return ports.Output{}, false
}

func (c *chain) spend(outpoint string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for script, utxos := range c.utxos {
		if i := slices.IndexFunc(
			utxos, func(u ports.Output) bool { return u.Outpoint == outpoint },
		); i >= 0 {
			c.utxos[script] = slices.Delete(utxos, i, i+1)
			c.push(script)
		}
	}
}

func (c *chain) tip() delegationdomain.Tip {
	return delegationdomain.Tip{
		Height: c.height, Time: c.times[c.height], MedianTime: c.times[c.height-5],
	}
}

func (c *chain) status(script string) string {
	utxos := slices.Clone(c.utxos[script])
	if len(utxos) == 0 {
		return ""
	}
	slices.SortFunc(
		utxos, func(a, b ports.Output) int { return strings.Compare(a.Outpoint, b.Outpoint) },
	)
	h := sha256.New()
	for _, u := range utxos {
		_, _ = fmt.Fprintf(h, "%s:%d:%d;", u.Outpoint, u.Amount, u.Height)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (c *chain) push(script string) {
	if c.sub != nil && !c.down {
		c.sub.status(script)
	}
}

func (c *chain) pushTip() {
	if c.sub != nil && !c.down {
		tip := c.tip()
		c.sub.emit(ports.ChainEvent{Tip: &tip})
	}
}

// resync pushes the tip and every script's status; the caller holds c.mu.
func (s *chainSub) resync() {
	s.c.pushTip()
	for _, script := range slices.Sorted(maps.Keys(s.scripts)) {
		s.c.push(script)
	}
}

// status pushes script's status when the subscription follows it; the caller holds c.mu.
func (s *chainSub) status(script string) {
	if _, ok := s.scripts[script]; ok {
		s.emit(ports.ChainEvent{Script: script, Status: s.c.status(script)})
	}
}

func (s *chainSub) emit(ev ports.ChainEvent) {
	select {
	case s.events <- ev:
	case <-s.ctx.Done():
	}
}

func newChain() *chain {
	c := &chain{
		utxos:        map[string][]ports.Output{},
		txs:          map[string]*wire.MsgTx{},
		times:        map[int64]time.Time{},
		height:       100,
		unspentCalls: map[string]int{},
	}
	for h := c.height - 5; h <= c.height; h++ {
		c.times[h] = time.Now()
	}
	return c
}
