package electrum

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/internal/core/ports"
	"github.com/btcsuite/btcd/wire/v2"
	log "github.com/sirupsen/logrus"
)

const (
	headerSize   = 80
	timeOffset   = 68 // of a header's timestamp
	medianBlocks = 11
	eventsBuffer = 256
)

const feeTarget = 2

var errSubscribed = errors.New("electrum watcher already has a subscription")

// Watcher is the chain watcher over one Electrum server.
type Watcher struct {
	c      *client
	syncMu sync.Mutex

	mu     sync.Mutex
	hashes map[string]string
	sub    *subscription
}

type subscription struct {
	w      *Watcher
	ctx    context.Context
	cancel context.CancelFunc
	events chan ports.ChainEvent
}

func NewWatcher(rawURL string) (*Watcher, error) { return newWatcher(rawURL, defaultTimings) }

func (w *Watcher) Close() { w.c.close() }

func (w *Watcher) Subscribe(
	ctx context.Context, scripts []string,
) (ports.ChainSubscription, error) {
	hashes, err := scriptHashes(scripts)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.sub != nil {
		return nil, errSubscribed
	}
	ctx, cancel := context.WithCancel(ctx)
	w.sub = &subscription{
		w: w, ctx: ctx, cancel: cancel, events: make(chan ports.ChainEvent, eventsBuffer),
	}
	for i, h := range hashes {
		w.hashes[h] = scripts[i]
	}
	go func() {
		if err := w.resync(ctx); err != nil && ctx.Err() == nil {
			log.WithError(err).Warn("electrum subscribe")
			w.c.reset()
		}
	}()
	return w.sub, nil
}

func (w *Watcher) Unspents(ctx context.Context, script string) ([]ports.Output, error) {
	hash, err := scriptHash(script)
	if err != nil {
		return nil, err
	}
	var items []struct {
		TxHash string `json:"tx_hash"`
		TxPos  uint32 `json:"tx_pos"`
		Height int64  `json:"height"`
		Value  uint64 `json:"value"`
	}
	if err := w.callInto(ctx, &items, "blockchain.scripthash.listunspent", hash); err != nil {
		return nil, err
	}
	out := make([]ports.Output, 0, len(items))
	for _, it := range items {
		out = append(out, ports.Output{
			Outpoint: fmt.Sprintf(
				"%s:%d", it.TxHash, it.TxPos,
			),
			Script: script,
			Amount: it.Value,
			Height: max(it.Height, 0),
		})
	}
	return out, nil
}

func (w *Watcher) MedianTime(ctx context.Context, height int64) (time.Time, error) {
	tip, err := w.tipAt(ctx, height)
	if err != nil {
		return time.Time{}, err
	}
	return tip.MedianTime, nil
}

func (w *Watcher) Tip(ctx context.Context) (delegation.Tip, error) {
	var head struct {
		Height int64 `json:"height"`
	}
	if err := w.callInto(ctx, &head, "blockchain.headers.subscribe"); err != nil {
		return delegation.Tip{}, err
	}
	return w.tipAt(ctx, head.Height)
}

func (w *Watcher) Spender(ctx context.Context, script, outpoint string) (string, error) {
	hash, err := scriptHash(script)
	if err != nil {
		return "", err
	}
	var hist []struct {
		TxHash string `json:"tx_hash"`
	}
	if err := w.callInto(ctx, &hist, "blockchain.scripthash.get_history", hash); err != nil {
		return "", err
	}
	if len(hist) > 64 {
		hist = hist[len(hist)-64:]
	}
	for i := len(hist) - 1; i >= 0; i-- {
		tx, err := w.Transaction(ctx, hist[i].TxHash)
		if err != nil {
			continue
		}
		for _, in := range tx.TxIn {
			if in.PreviousOutPoint.String() == outpoint {
				return hist[i].TxHash, nil
			}
		}
	}
	return "", nil
}

func (w *Watcher) Transaction(ctx context.Context, txid string) (*wire.MsgTx, error) {
	var rawTx string
	if err := w.callInto(ctx, &rawTx, "blockchain.transaction.get", txid); err != nil {
		return nil, err
	}
	b, err := hex.DecodeString(rawTx)
	if err != nil {
		return nil, err
	}
	tx := &wire.MsgTx{}
	if err := tx.Deserialize(bytes.NewReader(b)); err != nil {
		return nil, err
	}
	if got := tx.TxHash().String(); got != txid {
		return nil, fmt.Errorf("electrum transaction %s: server sent %s", txid, got)
	}
	return tx, nil
}

func (w *Watcher) Broadcast(ctx context.Context, tx *wire.MsgTx) (string, error) {
	var raw bytes.Buffer
	if err := tx.Serialize(&raw); err != nil {
		return "", err
	}
	var txid string
	err := w.callInto(
		ctx, &txid, "blockchain.transaction.broadcast", hex.EncodeToString(raw.Bytes()),
	)
	return txid, err
}

// FeeRate asks for the fee of a two block target, which the server gives in BTC per kvB, negative
// without an estimate.
func (w *Watcher) FeeRate(ctx context.Context) (float64, error) {
	var btcPerKvB float64
	if err := w.callInto(ctx, &btcPerKvB, "blockchain.estimatefee", feeTarget); err != nil {
		return 0, err
	}
	return max(btcPerKvB*1e8/1000, 1), nil
}

func (s *subscription) Events() <-chan ports.ChainEvent { return s.events }
func (s *subscription) Connected() bool                 { return s.w.c.connected() }

func (s *subscription) Update(ctx context.Context, add, remove []string) error {
	addHashes, err := scriptHashes(add)
	if err != nil {
		return err
	}
	removeHashes, err := scriptHashes(remove)
	if err != nil {
		return err
	}
	s.w.mu.Lock()
	for i, h := range addHashes {
		s.w.hashes[h] = add[i]
	}
	for _, h := range removeHashes {
		delete(s.w.hashes, h)
	}
	s.w.mu.Unlock()
	// a script added while disconnected is subscribed by the resync that follows the reconnect
	if err := s.w.subscribeHashes(
		ctx, addHashes,
	); err != nil &&
		!errors.Is(err, ports.ErrDisconnected) {
		return err
	}
	for chunk := range slices.Chunk(removeHashes, ports.MaxChainScripts) {
		calls := make([]call, len(chunk))
		for i, h := range chunk {
			calls[i] = call{method: "blockchain.scripthash.unsubscribe", params: []any{h}}
		}
		if _, err := s.w.c.batch(ctx, calls); err != nil {
			log.WithError(err).Debug("electrum unsubscribe")
		}
	}
	return nil
}

func (s *subscription) Stop() {
	s.cancel()
	s.w.mu.Lock()
	defer s.w.mu.Unlock()
	if s.w.sub == s {
		s.w.sub, s.w.hashes = nil, map[string]string{}
	}
}

func (w *Watcher) callInto(ctx context.Context, out any, method string, params ...any) error {
	raw, err := w.c.call(ctx, method, params...)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

func (w *Watcher) resync(ctx context.Context) error {
	w.syncMu.Lock()
	defer w.syncMu.Unlock()
	w.mu.Lock()
	sub := w.sub
	hashes := make([]string, 0, len(w.hashes))
	for h := range w.hashes {
		hashes = append(hashes, h)
	}
	w.mu.Unlock()
	if sub == nil {
		return nil
	}
	tip, err := w.Tip(ctx)
	if err != nil {
		return err
	}
	if !sub.emit(ports.ChainEvent{Tip: &tip}) {
		return nil
	}
	return w.subscribeHashes(ctx, hashes)
}

func (w *Watcher) subscribeHashes(ctx context.Context, hashes []string) error {
	for chunk := range slices.Chunk(hashes, ports.MaxChainScripts) {
		calls := make([]call, len(chunk))
		for i, h := range chunk {
			calls[i] = call{method: "blockchain.scripthash.subscribe", params: []any{h}}
		}
		results, err := w.c.batch(ctx, calls)
		if err != nil {
			return err
		}
		for i, raw := range results {
			var status *string
			if err := json.Unmarshal(raw, &status); err != nil {
				return err
			}
			w.pushStatus(chunk[i], status)
		}
	}
	return nil
}

func (w *Watcher) pushStatus(hash string, status *string) {
	w.mu.Lock()
	script, sub := w.hashes[hash], w.sub
	w.mu.Unlock()
	if sub == nil || script == "" {
		return
	}
	ev := ports.ChainEvent{Script: script}
	if status != nil {
		ev.Status = *status
	}
	sub.emit(ev)
}

func (w *Watcher) notify(method string, params json.RawMessage) {
	ctx := w.c.ctx
	switch method {
	case "blockchain.headers.subscribe":
		var p []struct {
			Height int64 `json:"height"`
		}
		if err := json.Unmarshal(params, &p); err != nil || len(p) == 0 {
			return
		}
		tip, err := w.tipAt(ctx, p[0].Height)
		if err != nil {
			if ctx.Err() == nil {
				log.WithError(err).Warn("electrum tip")
			}
			return
		}
		w.mu.Lock()
		sub := w.sub
		w.mu.Unlock()
		if sub != nil {
			sub.emit(ports.ChainEvent{Tip: &tip})
		}
	case "blockchain.scripthash.subscribe":
		var p []*string
		if err := json.Unmarshal(params, &p); err != nil || len(p) != 2 || p[0] == nil {
			return
		}
		w.pushStatus(*p[0], p[1])
	}
}

func (w *Watcher) tipAt(ctx context.Context, height int64) (delegation.Tip, error) {
	start := max(height-medianBlocks+1, 0)
	var res struct {
		Hex string `json:"hex"`
	}
	if err := w.callInto(ctx, &res, "blockchain.block.headers", start, height-start+1); err != nil {
		return delegation.Tip{}, err
	}
	raw, err := hex.DecodeString(res.Hex)
	if err != nil || len(raw) == 0 || len(raw)%headerSize != 0 {
		return delegation.Tip{}, fmt.Errorf("electrum headers at %d: malformed", start)
	}
	times := make([]time.Time, 0, len(raw)/headerSize)
	for i := 0; i < len(raw); i += headerSize {
		times = append(times, time.Unix(int64(binary.LittleEndian.Uint32(raw[i+timeOffset:])), 0))
	}
	tip := delegation.Tip{Height: height, Time: times[len(times)-1]}
	slices.SortFunc(times, time.Time.Compare)
	tip.MedianTime = times[len(times)/2]
	return tip, nil
}

func (s *subscription) emit(ev ports.ChainEvent) bool {
	select {
	case s.events <- ev:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func newWatcher(rawURL string, times timings) (*Watcher, error) {
	c, err := newClient(rawURL, times)
	if err != nil {
		return nil, err
	}
	w := &Watcher{c: c, hashes: map[string]string{}}
	c.onNotify, c.onReconnect = w.notify, w.resync
	c.start()
	return w, nil
}

func scriptHashes(scripts []string) ([]string, error) {
	out := make([]string, len(scripts))
	for i, script := range scripts {
		var err error
		if out[i], err = scriptHash(script); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scriptHash(script string) (string, error) {
	b, err := hex.DecodeString(strings.TrimSpace(script))
	if err != nil {
		return "", fmt.Errorf("script %q: %w", script, err)
	}
	sum := sha256.Sum256(b)
	slices.Reverse(sum[:])
	return hex.EncodeToString(sum[:]), nil
}
