package postgres

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/internal/infrastructure/postgres/sqlc/queries"
)

const (
	candidatesPerInput  = 4
	tuplesPerDelegation = 16
)

type CoinRepository struct{ db *DB }

func (d *DB) Coins() *CoinRepository { return &CoinRepository{db: d} }

func (r *CoinRepository) Upsert(ctx context.Context, coins ...delegation.Coin) error {
	if len(coins) == 0 {
		return nil
	}
	var orphans []string
	err := r.db.inTx(ctx, func(q *queries.Queries) error {
		for _, c := range coins {
			if _, err := q.LockDelegation(ctx, c.DelegationID); errors.Is(err, sql.ErrNoRows) {
				orphans = append(orphans, c.Outpoint)
				continue
			} else if err != nil {
				return err
			}
			stored, err := q.SelectCoinOnchain(ctx, c.Outpoint)
			switch {
			case errors.Is(err, sql.ErrNoRows):
			case err != nil:
				return err
			case stored != c.Onchain:
				return fmt.Errorf("%w: %s", delegation.ErrDomainClash, c.Outpoint)
			}
			err = q.UpsertCoin(ctx, queries.UpsertCoinParams{
				Outpoint:     c.Outpoint,
				DelegationID: c.DelegationID,
				Input:        int32(c.Input),
				Script:       c.Script,
				Amount: int64(
					c.Amount,
				),
				Expiry:    nullTime(c.ExpiresAt),
				CreatedAt: c.CreatedAt,
				Due:       nullTime(c.Due),
				Status: string(
					c.Status,
				),
				SpentBy:       c.SpentBy,
				Onchain:       c.Onchain,
				Height:        c.Height,
				Confirmations: int32(c.Confirmations),
				ConfirmedMtp:  nullTime(c.ConfirmedMTP),
			})
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil && len(orphans) > 0 {
		return fmt.Errorf(
			"%w: of %s", delegation.ErrDelegationNotFound, strings.Join(orphans, ", "),
		)
	}
	return err
}

func (r *CoinRepository) ListByDelegation(
	ctx context.Context, id int64,
) ([]delegation.Coin, error) {
	rows, err := r.db.querier.ListCoinsByDelegation(ctx, id)
	return toCoins(rows), err
}

func (r *CoinRepository) ScriptStatus(ctx context.Context, script string) (string, bool, error) {
	row, err := r.db.querier.GetWatchedStatus(ctx, script)
	return row.Status, row.Watched, err
}

func (r *CoinRepository) SetScriptStatus(ctx context.Context, script, status string) error {
	return r.db.querier.UpsertScriptStatus(
		ctx, queries.UpsertScriptStatusParams{Script: script, Status: status},
	)
}

func (r *CoinRepository) Move(
	ctx context.Context, script string, owner delegation.Claim,
) ([]delegation.Coin, error) {
	rows, err := r.db.querier.MoveCoins(
		ctx,
		queries.MoveCoinsParams{
			Script: script, Onchain: owner.Onchain, DelegationID: owner.DelegationID,
			Input: int32(owner.Input),
		},
	)
	return toCoins(rows), err
}

func (r *CoinRepository) Hold(ctx context.Context, outpoints []string) error {
	return r.db.querier.HoldCoins(ctx, outpoints)
}

func (r *CoinRepository) Release(ctx context.Context, outpoints []string, readyAt time.Time) error {
	return r.db.querier.ReleaseCoins(
		ctx, queries.ReleaseCoinsParams{RetryAt: nullTime(readyAt), Outpoints: outpoints},
	)
}

func (r *CoinRepository) ResetHolds(ctx context.Context) error {
	return r.db.querier.ResetHolds(ctx)
}

func (r *CoinRepository) Ready(
	ctx context.Context, now time.Time, tip delegation.Tip, limit int,
) ([][]delegation.Coin, error) {
	rows, err := r.db.querier.ListReadyCoins(ctx, queries.ListReadyCoinsParams{
		PerInput: candidatesPerInput, Now: now, MedianTime: tip.MedianTime.Unix(),
		Height: tip.Height,
	})
	if err != nil {
		return nil, err
	}
	blocked, err := r.activeBackoffs(ctx, now)
	if err != nil {
		return nil, err
	}
	return pickTuples(toCoins(rows), blocked, limit), nil
}

func (r *CoinRepository) Upcoming(
	ctx context.Context, until time.Time, tip delegation.Tip, limit int,
) ([][]delegation.Coin, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.db.querier.ListReadyCoins(ctx, queries.ListReadyCoinsParams{
		PerInput: int32(min(limit, math.MaxInt32)), Now: until,
		MedianTime: tip.MedianTime.Unix(), Height: tip.Height,
	})
	if err != nil {
		return nil, err
	}
	return everyTuple(toCoins(rows), limit), nil
}

func (r *CoinRepository) activeBackoffs(
	ctx context.Context, now time.Time,
) (map[string]bool, error) {
	rows, err := r.db.querier.ListActiveBackoffs(ctx, now)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, row := range rows {
		out[backoffKey(row.DelegationID, row.Coins)] = true
	}
	return out, nil
}

func (r *CoinRepository) Backoff(
	ctx context.Context, delegationID int64, coins []string, until time.Time,
) error {
	if len(coins) == 0 {
		return nil
	}
	return r.db.querier.UpsertTupleBackoff(ctx, queries.UpsertTupleBackoffParams{
		DelegationID: delegationID, Coins: coins, RetryAt: until,
	})
}

func (r *CoinRepository) ClearBackoff(ctx context.Context, outpoint string) error {
	return r.db.querier.DeleteBackoffsForOutpoint(ctx, outpoint)
}

func (r *CoinRepository) ClearDelegationBackoff(ctx context.Context, delegationID int64) error {
	return r.db.querier.DeleteBackoffsForDelegation(ctx, delegationID)
}

func (r *CoinRepository) NextBackoff(ctx context.Context, now time.Time) (time.Time, error) {
	at, err := r.db.querier.NextBackoffAt(ctx, now)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return at.UTC(), err
}

func (r *CoinRepository) NextReady(
	ctx context.Context, now time.Time, tip delegation.Tip,
) (time.Time, error) {
	at, err := r.db.querier.NextReadyAt(
		ctx,
		queries.NextReadyAtParams{Now: now, MedianTime: tip.MedianTime.Unix(), Height: tip.Height},
	)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	return at.Time.UTC(), err
}

func (r *CoinRepository) Totals(ctx context.Context) (int, uint64, error) {
	row, err := r.db.querier.TotalPresentCoins(ctx)
	return int(row.Coins), uint64(row.Amount), err
}

func (r *CoinRepository) Watched(
	ctx context.Context, scripts []string,
) ([]delegation.Watched, error) {
	claims, err := r.db.querier.ListClaimsAtScripts(ctx, scripts)
	if err != nil || len(claims) == 0 {
		return nil, err
	}
	coins, err := r.db.querier.ListCoinsAtScripts(ctx, scripts)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*delegation.Watched{}
	for _, row := range claims {
		key := scriptKey(row.Script, row.Onchain)
		w := byKey[key]
		if w == nil {
			w = &delegation.Watched{Script: row.Script, Onchain: row.Onchain}
			byKey[key] = w
		}
		w.Claims = append(w.Claims, delegation.Claim{
			DelegationID: row.DelegationID, Input: int(row.Input), Onchain: row.Onchain,
			Spawned: row.Spawned, Locktime: arklib.AbsoluteLocktime(row.Locktime),
			Address: row.Address, Sequence: uint32(row.Sequence),
			TxLocktime: uint32(row.TxLocktime),
		})
	}
	for _, c := range toCoins(coins) {
		if w := byKey[scriptKey(c.Script, c.Onchain)]; w != nil {
			w.Coins = append(w.Coins, c)
		}
	}
	out := make([]delegation.Watched, 0, len(byKey))
	for _, script := range scripts {
		for _, onchain := range []bool{false, true} {
			if w := byKey[scriptKey(script, onchain)]; w != nil {
				out = append(out, *w)
			}
		}
	}
	return out, nil
}

func (r *CoinRepository) Stranded(ctx context.Context) ([]string, error) {
	return r.db.querier.ListStrandedScripts(ctx)
}

func (r *CoinRepository) Onchain(ctx context.Context) ([]delegation.Coin, error) {
	rows, err := r.db.querier.ListOnchainCoins(ctx)
	return toCoins(rows), err
}

func (r *CoinRepository) Scripts(ctx context.Context, onchain bool) ([]string, error) {
	return r.db.querier.ListWatchedScripts(ctx, onchain)
}

func toCoins(rows []queries.Coin) []delegation.Coin {
	out := make([]delegation.Coin, 0, len(rows))
	for _, row := range rows {
		out = append(out, delegation.Coin{
			Outpoint:     row.Outpoint,
			DelegationID: row.DelegationID,
			Input:        int(row.Input),
			Script:       row.Script,
			Amount: uint64(
				row.Amount,
			),
			ExpiresAt: row.Expiry.Time.UTC(),
			CreatedAt: row.CreatedAt.UTC(),
			Due:       row.Due.Time.UTC(),
			Status: delegation.CoinStatus(
				row.Status,
			),
			SpentBy:       row.SpentBy,
			Onchain:       row.Onchain,
			Height:        row.Height,
			Confirmations: int(row.Confirmations),
			ConfirmedMTP:  utcTime(row.ConfirmedMtp),
		})
	}
	return out
}

func utcTime(t sql.NullTime) time.Time {
	if !t.Valid {
		return time.Time{}
	}
	return t.Time.UTC()
}

func scriptKey(script string, onchain bool) string {
	if onchain {
		return script + ":1"
	}
	return script + ":0"
}

func backoffKey(delegationID int64, coins []string) string {
	return fmt.Sprintf("%d\x00%s", delegationID, strings.Join(coins, "\x00"))
}

func pickTuples(coins []delegation.Coin, blocked map[string]bool, limit int) [][]delegation.Coin {
	if limit <= 0 || len(coins) == 0 {
		return nil
	}
	var out [][]delegation.Coin
	for _, g := range groupByInput(coins, candidatesPerInput) {
		tuple := firstTuple(g.id, g.slots, blocked)
		if tuple == nil {
			continue
		}
		out = append(out, tuple)
		if len(out) == limit {
			break
		}
	}
	return out
}

func everyTuple(coins []delegation.Coin, limit int) [][]delegation.Coin {
	var out [][]delegation.Coin
	for _, g := range groupByInput(coins, limit) {
		tuples := len(g.slots[0])
		for _, slot := range g.slots {
			slices.SortStableFunc(
				slot, func(a, b delegation.Coin) int { return a.Due.Compare(b.Due) },
			)
			tuples = min(tuples, len(slot))
		}
		for n := range tuples {
			tuple := make([]delegation.Coin, len(g.slots))
			for i, slot := range g.slots {
				tuple[i] = slot[n]
			}
			if !feasibleTuple(tuple) {
				continue
			}
			out = append(out, tuple)
			if len(out) == limit {
				return out
			}
		}
	}
	return out
}

type inputSlots struct {
	id    int64
	slots [][]delegation.Coin
}

func groupByInput(coins []delegation.Coin, perInput int) []inputSlots {
	var groups []inputSlots
	index := map[int64]int{}
	for _, c := range coins {
		i, ok := index[c.DelegationID]
		if !ok {
			i = len(groups)
			index[c.DelegationID] = i
			groups = append(groups, inputSlots{id: c.DelegationID})
		}
		g := &groups[i]
		for len(g.slots) <= c.Input {
			g.slots = append(g.slots, nil)
		}
		if len(g.slots[c.Input]) < perInput {
			g.slots[c.Input] = append(g.slots[c.Input], c)
		}
	}
	return groups
}

func firstTuple(id int64, slots [][]delegation.Coin, blocked map[string]bool) []delegation.Coin {
	if len(slots) == 0 {
		return nil
	}
	for _, slot := range slots {
		if len(slot) == 0 {
			return nil
		}
	}
	idx := make([]int, len(slots))
	for n := 0; n < tuplesPerDelegation; n++ {
		tuple := make([]delegation.Coin, len(slots))
		ops := make([]string, len(slots))
		for i, slot := range slots {
			tuple[i] = slot[idx[i]]
			ops[i] = tuple[i].Outpoint
		}
		if !blocked[backoffKey(id, ops)] && feasibleTuple(tuple) {
			return tuple
		}
		k := len(idx) - 1
		for k >= 0 {
			idx[k]++
			if idx[k] < len(slots[k]) {
				break
			}
			idx[k] = 0
			k--
		}
		if k < 0 {
			return nil
		}
	}
	return nil
}

func feasibleTuple(coins []delegation.Coin) bool {
	var due time.Time
	for _, c := range coins {
		if c.Due.After(due) {
			due = c.Due
		}
	}
	for _, c := range coins {
		if !c.Onchain && !c.ExpiresAt.IsZero() && !c.ExpiresAt.After(due) {
			return false
		}
	}
	return true
}

func nullTime(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: !t.IsZero()} }
