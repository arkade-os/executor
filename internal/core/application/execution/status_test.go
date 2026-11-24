package execution

import (
	"context"
	"testing"
	"time"

	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/internal/core/domain/execution"
	"github.com/stretchr/testify/require"
)

func TestAliveFailsWhenWorkIsDueAndTheLoopIsSilent(t *testing.T) {
	s := NewService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, Limits{LoopStall: time.Minute},
	)
	now := time.Unix(1_800_000_000, 0)

	require.NoError(t, s.Alive(now), "nothing is armed")

	s.wake.Store(now.Add(time.Hour).UnixNano())
	require.NoError(t, s.Alive(now.Add(time.Hour)), "work is not due before the timer fires")
	require.NoError(t, s.Alive(now.Add(time.Hour+30*time.Second)), "within the bound")
	require.Error(t, s.Alive(now.Add(time.Hour+2*time.Minute)))

	s.beat.Store(now.Add(time.Hour + 90*time.Second).UnixNano())
	require.NoError(
		t, s.Alive(now.Add(time.Hour+2*time.Minute)), "the loop handled something since",
	)
}

func TestAliveFailsOnceTheBatchEventsAreLostForLong(t *testing.T) {
	s := NewService(
		nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, Limits{LoopStall: time.Minute},
	)
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name   string
		lostAt time.Time
		alive  bool
	}{
		{name: "followed", alive: true},
		{name: "lost within the bound", lostAt: now.Add(-30 * time.Second), alive: true},
		{name: "lost for longer", lostAt: now.Add(-2 * time.Minute)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.batch.lostAtNano.Store(0)
			if !tt.lostAt.IsZero() {
				s.batch.lostAtNano.Store(tt.lostAt.UnixNano())
			}
			require.Equal(t, tt.alive, s.Alive(now) == nil)
		})
	}
}

func TestForecast(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	coin := func(outpoint string, due time.Duration) delegation.Coin {
		return delegation.Coin{
			Outpoint: outpoint, DelegationID: 2, Amount: 1000, Due: now.Add(due),
			ExpiresAt: now.Add(due + day),
		}
	}
	s := &Service{
		coins: upcomingCoins{tuples: [][]delegation.Coin{
			{coin("a:0", 24*day)}, {coin("b:0", 29*day)},
		}},
		session: execution.Session{Duration: time.Minute, MaxIntents: 128},
		limits:  Limits{Reserve: 15 * time.Minute},
	}
	var st Status
	require.NoError(t, s.forecast(t.Context(), &st))

	require.Len(t, st.Batches, 2, "a batch for each coin of the delegation")
	for i, due := range []time.Duration{24 * day, 29 * day} {
		require.Equal(t, 1, st.Batches[i].Coins)
		require.Equal(t, int64(2), st.Batches[i].Entries[0].DelegationID)
		require.WithinDuration(t, now.Add(due), st.Batches[i].Entries[0].Due, 0)
		require.False(t, st.Batches[i].At.Before(now.Add(due)))
		require.False(t, st.Batches[i].At.After(now.Add(due+day)))
	}
	require.Zero(t, st.Late)
}

type upcomingCoins struct {
	delegation.CoinRepository
	tuples [][]delegation.Coin
}

func (u upcomingCoins) Upcoming(
	context.Context, time.Time, delegation.Tip, int,
) ([][]delegation.Coin, error) {
	return u.tuples, nil
}
