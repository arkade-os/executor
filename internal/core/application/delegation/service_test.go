package delegation

import (
	"context"
	"testing"
	"time"

	arklib "github.com/arkade-os/arkd/pkg/ark-lib"
	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/pkg/template/ecies"
	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/stretchr/testify/require"
)

func TestAddReturnsTheActiveOne(t *testing.T) {
	s := &Service{delegations: &rows{}}
	first, err := s.add(t.Context(), testDelegation("a", 0), 0)
	require.NoError(t, err)
	again, err := s.add(t.Context(), testDelegation("a", 0), 0)
	require.NoError(t, err)
	require.Equal(t, first.ID, again.ID)
}

func TestAddBlockedAfterCancelNotAfterExpiry(t *testing.T) {
	repo := &rows{}
	s := &Service{delegations: repo}
	first, err := s.add(t.Context(), testDelegation("a", 0), 0)
	require.NoError(t, err)

	repo.list[0].Status = delegation.StatusCancelled
	_, err = s.add(t.Context(), testDelegation("a", 0), 0)
	require.ErrorIs(t, err, delegation.ErrBlocked)

	repo.list[0].Status = delegation.StatusExpired
	second, err := s.add(t.Context(), testDelegation("a", 0), 0)
	require.NoError(t, err)
	require.NotEqual(t, first.ID, second.ID)
}

func TestAddCapsOriginsApart(t *testing.T) {
	repo := &rows{}
	s := &Service{delegations: repo}
	_, err := s.add(t.Context(), testDelegation("a", 0), 1)
	require.NoError(t, err)
	_, err = s.add(t.Context(), testDelegation("b", 0), 1)
	require.ErrorIs(t, err, delegation.ErrCapReached)
	_, err = s.add(t.Context(), testDelegation("a", 0), 1)
	require.NoError(t, err, "the active one still comes back at the cap")

	_, err = s.add(t.Context(), testDelegation("c", 7), 1)
	require.NoError(t, err, "a spawned delegation is not counted with the others")
	_, err = s.add(t.Context(), testDelegation("d", 7), 1)
	require.ErrorIs(t, err, delegation.ErrCapReached)

	repo.list[0].Status = delegation.StatusExpired
	_, err = s.add(t.Context(), testDelegation("b", 0), 1)
	require.NoError(t, err, "only active delegations count")
}

func TestAddNotifiesWhatItEvicts(t *testing.T) {
	repo := &rows{}
	s := &Service{delegations: repo, limits: Limits{EvictIdleAfter: time.Hour}}
	var notified []delegation.Delegation
	s.OnChange(func(d delegation.Delegation) { notified = append(notified, d) })
	first, err := s.add(t.Context(), testDelegation("a", 0), 1)
	require.NoError(t, err)
	repo.idle = []delegation.Delegation{*first}

	_, err = s.add(t.Context(), testDelegation("b", 0), 1)
	require.NoError(t, err)
	require.Len(t, notified, 1)
	require.Equal(t, first.ID, notified[0].ID)
	require.Equal(t, delegation.StatusExpired, notified[0].Status, "so its scripts are forgotten")
}

func TestMakeRoom(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	limits := Limits{EvictIdleAfter: time.Hour, ExpiryInterval: time.Minute}
	coinless := []delegation.Delegation{{ID: 1}}
	tests := []struct {
		name         string
		spawned      bool
		limits       Limits
		coinless     []delegation.Delegation
		err          error
		evictBefore  time.Time
		settledSince time.Time
	}{
		{
			name: "a client's that never held a coin goes", limits: limits, coinless: coinless,
			evictBefore: now.Add(-time.Hour),
		},
		{
			name: "a spawned one without an unspent coin goes", spawned: true, limits: limits,
			coinless: coinless, settledSince: now.Add(-time.Minute),
		},
		{
			name: "none serves no coin", limits: limits, err: delegation.ErrCapReached,
			evictBefore: now.Add(-time.Hour),
		},
		{
			name: "clients are kept", limits: Limits{ExpiryInterval: time.Minute},
			coinless: coinless, err: delegation.ErrCapReached,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &rows{idle: tt.coinless, settled: tt.coinless}
			s := &Service{delegations: repo, limits: tt.limits}
			streamID := int64(0)
			if tt.spawned {
				streamID = 7
			}
			_, err := s.add(t.Context(), testDelegation("a", streamID), 1)
			require.NoError(t, err)

			evicted, err := s.makeRoom(t.Context(), tt.spawned, 1, now)
			require.ErrorIs(t, err, tt.err)
			require.Equal(t, tt.err == nil, len(evicted) == 1)
			require.Equal(t, tt.evictBefore, repo.evictBefore)
			require.Equal(t, tt.settledSince, repo.settledSince)
		})
	}

	t.Run("a search that fell short waits for the next expiry round", func(t *testing.T) {
		repo := &rows{}
		s := &Service{delegations: repo, limits: limits}
		_, err := s.add(t.Context(), testDelegation("a", 0), 1)
		require.NoError(t, err)
		_, err = s.makeRoom(t.Context(), false, 1, now)
		require.ErrorIs(t, err, delegation.ErrCapReached)

		repo.evictBefore = time.Time{}
		_, err = s.makeRoom(t.Context(), false, 1, now.Add(30*time.Second))
		require.ErrorIs(t, err, delegation.ErrCapReached)
		require.Zero(t, repo.evictBefore)
		_, err = s.makeRoom(t.Context(), false, 1, now.Add(time.Minute))
		require.ErrorIs(t, err, delegation.ErrCapReached)
		require.Equal(t, now.Add(time.Minute-time.Hour), repo.evictBefore)
	})
}

func TestExpireDelegations(t *testing.T) {
	now := time.Now()
	day := 24 * time.Hour
	tests := []struct {
		name            string
		limits          Limits
		count           int
		notified        []int64
		idleBefore      time.Time
		pruneBefore     time.Time
		pruneIdleBefore time.Time
	}{
		{
			name: "idle expiry off", limits: Limits{PruneAfter: day},
			count: 1, notified: []int64{1}, pruneBefore: now.Add(-day),
		},
		{
			name: "idle expiry on", limits: Limits{PruneAfter: day, IdleExpiry: 2 * day},
			count: 3, notified: []int64{1, 2, 3}, idleBefore: now.Add(-2 * day),
			pruneBefore: now.Add(-day), pruneIdleBefore: now.Add(-day),
		},
		{
			name: "pruning off", limits: Limits{IdleExpiry: 2 * day},
			count: 3, notified: []int64{1, 2, 3}, idleBefore: now.Add(-2 * day),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo := &rows{
				expired: []delegation.Delegation{{ID: 1}},
				idle:    []delegation.Delegation{{ID: 2}, {ID: 3}},
			}
			s := &Service{delegations: repo, limits: tt.limits}
			var notified []int64
			s.OnChange(func(d delegation.Delegation) { notified = append(notified, d.ID) })

			n, err := s.ExpireDelegations(t.Context(), now)
			require.NoError(t, err)
			require.Equal(t, tt.count, n)
			require.Equal(t, tt.notified, notified)
			require.Equal(t, tt.idleBefore, repo.idleBefore)
			if !tt.idleBefore.IsZero() {
				require.Equal(t, idleExpiryBatch, repo.idleLimit)
			}
			require.Equal(t, tt.pruneBefore, repo.pruneBefore)
			require.Equal(t, tt.pruneIdleBefore, repo.pruneIdleBefore)
		})
	}
}

func TestDecryptFor(t *testing.T) {
	key, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pub := key.PubKey()
	without := NewService(nil, nil, nil, nil, pub, nil, pub, pub, arklib.BitcoinRegTest, Limits{})
	require.Nil(t, without.decryptFor("template"))

	s := NewService(nil, nil, nil, nil, pub, key, pub, pub, arklib.BitcoinRegTest, Limits{})
	plaintext := []byte("secret")
	ciphertext, err := ecies.Encrypt(pub, plaintext, "template")
	require.NoError(t, err)
	decrypt := s.decryptFor("template")
	require.NotNil(t, decrypt)
	got, err := decrypt(ciphertext)
	require.NoError(t, err)
	require.Equal(t, plaintext, got)
	_, err = s.decryptFor("other template")(ciphertext)
	require.Error(t, err)
}

func testDelegation(fingerprint string, streamID int64) *delegation.Delegation {
	return &delegation.Delegation{
		Fingerprint: fingerprint, StreamID: streamID, Status: delegation.StatusActive,
	}
}

type rows struct {
	delegation.DelegationRepository
	list []delegation.Delegation

	expired, idle   []delegation.Delegation
	idleBefore      time.Time
	idleLimit       int
	evictBefore     time.Time
	settled         []delegation.Delegation
	settledSince    time.Time
	pruneBefore     time.Time
	pruneIdleBefore time.Time
}

func (r *rows) AddOrUpdate(_ context.Context, d *delegation.Delegation) error {
	d.ID = int64(len(r.list) + 1)
	r.list = append(r.list, *d)
	return nil
}

func (r *rows) GetByFingerprint(
	_ context.Context, fingerprint string,
) (*delegation.Delegation, error) {
	for i := len(r.list) - 1; i >= 0; i-- {
		if r.list[i].Fingerprint == fingerprint {
			v := r.list[i]
			return &v, nil
		}
	}
	return nil, delegation.ErrDelegationNotFound
}

func (r *rows) CountActive(context.Context) (clients, spawned int64, err error) {
	for _, d := range r.list {
		switch {
		case d.Status != delegation.StatusActive:
		case d.StreamID != 0:
			spawned++
		default:
			clients++
		}
	}
	return clients, spawned, nil
}

func (r *rows) Expire(context.Context, time.Time) ([]delegation.Delegation, error) {
	return r.expired, nil
}

func (r *rows) ExpireIdle(
	_ context.Context, registeredBefore time.Time, limit int,
) ([]delegation.Delegation, error) {
	r.idleBefore, r.idleLimit = registeredBefore, limit
	return r.idle, nil
}

func (r *rows) EvictIdle(
	_ context.Context, registeredBefore time.Time, _ int,
) ([]delegation.Delegation, error) {
	r.evictBefore = registeredBefore
	return r.idle, nil
}

func (r *rows) ExpireSettled(
	_ context.Context, quietSince time.Time, _ int,
) ([]delegation.Delegation, error) {
	r.settledSince = quietSince
	return r.settled, nil
}

func (r *rows) Prune(_ context.Context, before time.Time) (int64, error) {
	r.pruneBefore = before
	return 0, nil
}

func (r *rows) PruneIdle(_ context.Context, before time.Time) (int64, error) {
	r.pruneIdleBefore = before
	return 0, nil
}
