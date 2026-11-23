package postgres

import (
	"testing"
	"time"

	"github.com/arkade-os/executor/internal/core/domain/delegation"
	"github.com/arkade-os/executor/internal/infrastructure/postgres/sqlc/queries"
	"github.com/stretchr/testify/require"
)

func TestDelegationAddThenGet(t *testing.T) {
	repo := testDelegations(t)
	expires := time.Now().Add(time.Hour).Truncate(time.Microsecond)
	w := testDelegation("a")
	w.ExpiresAt = &expires
	created := w
	require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
	require.NotZero(t, created.ID)
	require.False(t, created.CreatedAt.IsZero())
	require.Equal(t, created.CreatedAt, created.UpdatedAt)
	require.Equal(t, delegation.StatusActive, created.Status)
	require.Equal(t, w.Inputs, created.Inputs)
	require.Equal(t, w.Variables, created.Variables)
	require.Equal(t, w.StreamID, created.StreamID)
	require.True(t, expires.Equal(*created.ExpiresAt))

	got, err := repo.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, &created, got)
	got, err = repo.GetByAddress(t.Context(), w.Inputs[0].Address)
	require.NoError(t, err)
	require.Equal(t, &created, got)
	got, err = repo.GetByFingerprint(t.Context(), w.Fingerprint)
	require.NoError(t, err)
	require.Equal(t, &created, got)
}

func TestDelegationOneActivePerFingerprint(t *testing.T) {
	repo := testDelegations(t)
	first := testDelegation("a")
	require.NoError(t, repo.AddOrUpdate(t.Context(), &first))
	again := testDelegation("a")
	require.Error(t, repo.AddOrUpdate(t.Context(), &again))

	first.Status = delegation.StatusExpired
	require.NoError(t, repo.AddOrUpdate(t.Context(), &first))
	second := testDelegation("a")
	require.NoError(t, repo.AddOrUpdate(t.Context(), &second))
	got, err := repo.GetByFingerprint(t.Context(), "a")
	require.NoError(t, err)
	require.Equal(t, second.ID, got.ID, "the newest one")
}

func TestDelegationGetByAddressPrefersActive(t *testing.T) {
	repo := testDelegations(t)
	first := testDelegation("a")
	require.NoError(t, repo.AddOrUpdate(t.Context(), &first))
	first.Status = delegation.StatusExpired
	require.NoError(t, repo.AddOrUpdate(t.Context(), &first))
	second := testDelegation("a")
	require.NoError(t, repo.AddOrUpdate(t.Context(), &second))
	second.Status = "done"
	require.NoError(t, repo.AddOrUpdate(t.Context(), &second))
	third := testDelegation("a")
	require.NoError(t, repo.AddOrUpdate(t.Context(), &third))
	third.Status = "done"
	require.NoError(t, repo.AddOrUpdate(t.Context(), &third))
	second.Status = delegation.StatusActive
	require.NoError(t, repo.AddOrUpdate(t.Context(), &second))

	got, err := repo.GetByAddress(t.Context(), first.Inputs[0].Address)
	require.NoError(t, err)
	require.Equal(t, second.ID, got.ID)
}

func TestDelegationGetByAddressFindsAnyInput(t *testing.T) {
	repo := testDelegations(t)
	d := testDelegation("a")
	d.Inputs = append(d.Inputs, delegation.Input{
		Name: "second", Script: "5120ab", Address: "addr-second", Sequence: 0xffffffff,
	})
	require.NoError(t, repo.AddOrUpdate(t.Context(), &d))
	got, err := repo.GetByAddress(t.Context(), "addr-second")
	require.NoError(t, err)
	require.Equal(t, d.ID, got.ID)
	got, err = repo.GetByAddress(t.Context(), d.Inputs[0].Address)
	require.NoError(t, err)
	require.Equal(t, d.ID, got.ID)
}

func TestDelegationCountActiveByOrigin(t *testing.T) {
	repo := testDelegations(t)
	stream := testStream()
	require.NoError(t, repo.db.Streams().AddOrUpdate(t.Context(), &stream))
	for _, d := range []delegation.Delegation{testDelegation("a"), testDelegation("b"), testSpawned("c", stream.ID)} {
		require.NoError(t, repo.AddOrUpdate(t.Context(), &d))
	}
	cancelled := testDelegation("d")
	require.NoError(t, repo.AddOrUpdate(t.Context(), &cancelled))
	cancelled.Status = delegation.StatusCancelled
	require.NoError(t, repo.AddOrUpdate(t.Context(), &cancelled))

	clients, spawned, err := repo.CountActive(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(2), clients)
	require.Equal(t, int64(1), spawned)
}

func TestDelegationUsage(t *testing.T) {
	repo := testDelegations(t)
	stream := testStream()
	require.NoError(t, repo.db.Streams().AddOrUpdate(t.Context(), &stream))
	ids := map[string]int64{}
	for _, name := range []string{"never funded", "holding", "emptied", "cancelled"} {
		created := testDelegation(name)
		require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
		ids[name] = created.ID
	}
	spawned := testSpawned("spawned", stream.ID)
	require.NoError(t, repo.AddOrUpdate(t.Context(), &spawned))
	cancelled := delegation.Delegation{ID: ids["cancelled"], Status: delegation.StatusCancelled}
	require.NoError(t, repo.AddOrUpdate(t.Context(), &cancelled))
	insertCoin(t, repo, "aa:0", ids["holding"], delegation.Present)
	insertCoin(t, repo, "aa:1", ids["holding"], delegation.Spent)
	insertCoin(t, repo, "bb:0", ids["emptied"], delegation.Spent)
	insertCoin(t, repo, "cc:0", spawned.ID, delegation.Present)

	usage, err := repo.Usage(t.Context())
	require.NoError(t, err)
	require.Equal(t, delegation.Usage{
		Clients: 3, Spawned: 1, ClientsHoldingCoin: 1, ClientsNeverFunded: 1,
	}, usage)
}

func TestDelegationPruneSpawnedExpired(t *testing.T) {
	repo := testDelegations(t)
	stream := testStream()
	require.NoError(t, repo.db.Streams().AddOrUpdate(t.Context(), &stream))
	past := time.Now().Add(-48 * time.Hour)
	old := withExpiry(testSpawned("old", stream.ID), &past)
	require.NoError(t, repo.AddOrUpdate(t.Context(), &old))
	fresh := withExpiry(testSpawned("fresh", stream.ID), &past)
	require.NoError(t, repo.AddOrUpdate(t.Context(), &fresh))
	live := testSpawned("live", stream.ID)
	require.NoError(t, repo.AddOrUpdate(t.Context(), &live))
	public := withExpiry(testDelegation("public"), &past)
	require.NoError(t, repo.AddOrUpdate(t.Context(), &public))
	_, err := repo.Expire(t.Context(), time.Now())
	require.NoError(t, err)
	_, err = repo.db.db.ExecContext(
		t.Context(), "UPDATE delegations SET expires_at = now() WHERE id = $1", fresh.ID,
	)
	require.NoError(t, err)
	_, err = repo.db.db.ExecContext(
		t.Context(),
		"INSERT INTO coins (outpoint, delegation_id, input, script, amount, created_at, status) VALUES ('aa:0', $1, 0, 's', 1, now(), 'present')",
		old.ID,
	)
	require.NoError(t, err)

	n, err := repo.Prune(t.Context(), time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, n, "only the spawned delegation expired before the time")
	_, err = repo.Get(t.Context(), old.ID)
	require.ErrorIs(t, err, delegation.ErrDelegationNotFound)
	for _, id := range []int64{fresh.ID, live.ID, public.ID} {
		_, err = repo.Get(t.Context(), id)
		require.NoError(t, err)
	}
	var coins int
	require.NoError(
		t, repo.db.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM coins").Scan(&coins),
	)
	require.Zero(t, coins, "its coins go with it")
}

func TestDelegationNotFound(t *testing.T) {
	repo := testDelegations(t)
	_, err := repo.Get(t.Context(), 1)
	require.ErrorIs(t, err, delegation.ErrDelegationNotFound)
	_, err = repo.GetByAddress(t.Context(), "nowhere")
	require.ErrorIs(t, err, delegation.ErrDelegationNotFound)
	_, err = repo.GetByFingerprint(t.Context(), "none")
	require.ErrorIs(t, err, delegation.ErrDelegationNotFound)
}

func TestDelegationUpdate(t *testing.T) {
	repo := testDelegations(t)
	created := testDelegation("a")
	require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
	created.Status = delegation.StatusCancelled
	created.Inputs[0].Address = "ignored"
	require.NoError(t, repo.AddOrUpdate(t.Context(), &created))

	got, err := repo.Get(t.Context(), created.ID)
	require.NoError(t, err)
	require.Equal(t, delegation.StatusCancelled, got.Status)
	require.Equal(t, testDelegation("a").Inputs[0].Address, got.Inputs[0].Address)
	require.True(t, got.UpdatedAt.After(got.CreatedAt))

	require.ErrorIs(
		t,
		repo.AddOrUpdate(
			t.Context(), &delegation.Delegation{ID: 404, Status: delegation.StatusActive},
		),
		delegation.ErrDelegationNotFound,
	)
}

func TestDelegationExpire(t *testing.T) {
	repo := testDelegations(t)
	now := time.Now()
	past, future := now.Add(-time.Minute), now.Add(time.Hour)
	ids := map[string]int64{}
	for name, w := range map[string]delegation.Delegation{
		"past":           withExpiry(testDelegation("past"), &past),
		"at":             withExpiry(testDelegation("at"), &now),
		"future":         withExpiry(testDelegation("future"), &future),
		"never":          testDelegation("never"),
		"cancelled past": withExpiry(testDelegation("cancelled"), &past),
	} {
		created := w
		require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
		ids[name] = created.ID
	}
	cancelled, err := repo.Get(t.Context(), ids["cancelled past"])
	require.NoError(t, err)
	require.NoError(t, cancelled.Cancel())
	require.NoError(t, repo.AddOrUpdate(t.Context(), cancelled))

	expired, err := repo.Expire(t.Context(), now)
	require.NoError(t, err)
	require.ElementsMatch(t, []int64{ids["past"], ids["at"]}, listIDs(expired))
	for _, d := range expired {
		require.Equal(t, delegation.StatusExpired, d.Status)
	}
	for name, status := range map[string]string{
		"past": delegation.StatusExpired, "at": delegation.StatusExpired, "future": delegation.StatusActive,
		"never": delegation.StatusActive, "cancelled past": delegation.StatusCancelled,
	} {
		got, err := repo.Get(t.Context(), ids[name])
		require.NoError(t, err)
		require.Equal(t, status, got.Status, name)
	}
	expired, err = repo.Expire(t.Context(), now)
	require.NoError(t, err)
	require.Empty(t, expired)
}

func TestDelegationExpireIdle(t *testing.T) {
	t.Run("never-funded old clients", func(t *testing.T) {
		repo := testDelegations(t)
		stream := testStream()
		require.NoError(t, repo.db.Streams().AddOrUpdate(t.Context(), &stream))
		ids := map[string]int64{}
		for _, name := range []string{"idle", "present coin", "spent coin", "recent", "cancelled"} {
			created := testDelegation(name)
			require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
			ids[name] = created.ID
		}
		spawned := testSpawned("spawned", stream.ID)
		require.NoError(t, repo.AddOrUpdate(t.Context(), &spawned))
		ids["spawned"] = spawned.ID
		cancelled, err := repo.Get(t.Context(), ids["cancelled"])
		require.NoError(t, err)
		require.NoError(t, cancelled.Cancel())
		require.NoError(t, repo.AddOrUpdate(t.Context(), cancelled))
		for name, id := range ids {
			if name != "recent" {
				backdate(t, repo, "created_at", id, 48*time.Hour)
			}
		}
		insertCoin(t, repo, "aa:0", ids["present coin"], delegation.Present)
		insertCoin(t, repo, "bb:0", ids["spent coin"], delegation.Spent)

		expired, err := repo.ExpireIdle(t.Context(), time.Now().Add(-24*time.Hour), 10)
		require.NoError(t, err)
		require.Equal(t, []int64{ids["idle"]}, listIDs(expired))
		require.Equal(t, delegation.StatusExpired, expired[0].Status)
		for name, status := range map[string]string{
			"idle": delegation.StatusExpired, "present coin": delegation.StatusActive,
			"spent coin": delegation.StatusActive, "recent": delegation.StatusActive,
			"cancelled": delegation.StatusCancelled, "spawned": delegation.StatusActive,
		} {
			got, err := repo.Get(t.Context(), ids[name])
			require.NoError(t, err)
			require.Equal(t, status, got.Status, name)
		}
		expired, err = repo.ExpireIdle(t.Context(), time.Now().Add(-24*time.Hour), 10)
		require.NoError(t, err)
		require.Empty(t, expired)
	})
	t.Run("at most limit", func(t *testing.T) {
		repo := testDelegations(t)
		var ids []int64
		for _, fp := range []string{"a", "b", "c"} {
			created := testDelegation(fp)
			require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
			backdate(t, repo, "created_at", created.ID, 48*time.Hour)
			ids = append(ids, created.ID)
		}

		expired, err := repo.ExpireIdle(t.Context(), time.Now().Add(-24*time.Hour), 2)
		require.NoError(t, err)
		require.Equal(t, ids[:2], listIDs(expired), "the oldest first")
		expired, err = repo.ExpireIdle(t.Context(), time.Now().Add(-24*time.Hour), 2)
		require.NoError(t, err)
		require.Equal(t, ids[2:], listIDs(expired), "the rest on the next call")
	})
}

func TestDelegationEvictIdle(t *testing.T) {
	t.Run("old clients that never held a coin nor made an attempt", func(t *testing.T) {
		repo := testDelegations(t)
		stream := testStream()
		require.NoError(t, repo.db.Streams().AddOrUpdate(t.Context(), &stream))
		ids := map[string]int64{}
		for _, name := range []string{"idle", "coin", "attempt", "recent", "cancelled"} {
			created := testDelegation(name)
			require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
			ids[name] = created.ID
		}
		spawned := testSpawned("spawned", stream.ID)
		require.NoError(t, repo.AddOrUpdate(t.Context(), &spawned))
		ids["spawned"] = spawned.ID
		cancelled := delegation.Delegation{ID: ids["cancelled"], Status: delegation.StatusCancelled}
		require.NoError(t, repo.AddOrUpdate(t.Context(), &cancelled))
		for name, id := range ids {
			if name != "recent" {
				backdate(t, repo, "created_at", id, 48*time.Hour)
			}
		}
		insertCoin(t, repo, "aa:0", ids["coin"], delegation.Spent)
		require.NoError(t, repo.db.Attempts().AddOrUpdate(t.Context(), &delegation.Attempt{
			DelegationID: ids["attempt"], Coins: []string{"moved:0"}, Status: delegation.Failed,
		}))

		evicted, err := repo.EvictIdle(t.Context(), time.Now().Add(-24*time.Hour), 10)
		require.NoError(t, err)
		require.Equal(t, []int64{ids["idle"]}, listIDs(evicted))
		require.Equal(t, "5120aa", evicted[0].Inputs[0].Script, "it comes back as it was")
		_, err = repo.Get(t.Context(), ids["idle"])
		require.ErrorIs(t, err, delegation.ErrDelegationNotFound)
		var claims int
		require.NoError(t, repo.db.db.QueryRowContext(
			t.Context(), "SELECT COUNT(*) FROM claims WHERE delegation_id = $1", ids["idle"],
		).Scan(&claims))
		require.Zero(t, claims, "its claims go with it")
		for _, name := range []string{"coin", "attempt", "recent", "cancelled", "spawned"} {
			_, err = repo.Get(t.Context(), ids[name])
			require.NoError(t, err, name)
		}
	})
	t.Run("at most limit", func(t *testing.T) {
		repo := testDelegations(t)
		var ids []int64
		for _, fp := range []string{"a", "b", "c"} {
			created := testDelegation(fp)
			require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
			backdate(t, repo, "created_at", created.ID, 48*time.Hour)
			ids = append(ids, created.ID)
		}

		evicted, err := repo.EvictIdle(t.Context(), time.Now().Add(-24*time.Hour), 2)
		require.NoError(t, err)
		require.ElementsMatch(t, ids[:2], listIDs(evicted), "the oldest first")
		evicted, err = repo.EvictIdle(t.Context(), time.Now().Add(-24*time.Hour), 2)
		require.NoError(t, err)
		require.Equal(t, ids[2:], listIDs(evicted), "the rest on the next call")
	})
	t.Run("not one a coin is being stored for", func(t *testing.T) {
		repo := testDelegations(t)
		created := testDelegation("a")
		require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
		backdate(t, repo, "created_at", created.ID, 48*time.Hour)

		require.NoError(t, repo.db.inTx(t.Context(), func(q *queries.Queries) error {
			if _, err := q.LockDelegation(t.Context(), created.ID); err != nil {
				return err
			}
			evicted, err := repo.EvictIdle(t.Context(), time.Now().Add(-24*time.Hour), 10)
			require.Empty(t, evicted)
			return err
		}))
		evicted, err := repo.EvictIdle(t.Context(), time.Now().Add(-24*time.Hour), 10)
		require.NoError(t, err)
		require.Equal(t, []int64{created.ID}, listIDs(evicted), "once the coins are stored")
	})
}

func TestDelegationExpireSettled(t *testing.T) {
	t.Run("spawned ones without an unspent coin, left alone", func(t *testing.T) {
		repo := testDelegations(t)
		stream := testStream()
		require.NoError(t, repo.db.Streams().AddOrUpdate(t.Context(), &stream))
		ids := map[string]int64{}
		for _, name := range []string{
			"spent", "never funded", "present coin", "just spent", "recent", "cancelled",
		} {
			created := testSpawned(name, stream.ID)
			require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
			ids[name] = created.ID
		}
		client := testDelegation("client")
		require.NoError(t, repo.AddOrUpdate(t.Context(), &client))
		ids["client"] = client.ID
		cancelled, err := repo.Get(t.Context(), ids["cancelled"])
		require.NoError(t, err)
		require.NoError(t, cancelled.Cancel())
		require.NoError(t, repo.AddOrUpdate(t.Context(), cancelled))
		for name, id := range ids {
			if name != "recent" {
				backdate(t, repo, "updated_at", id, 2*time.Hour)
			}
		}
		insertCoin(t, repo, "aa:0", ids["spent"], delegation.Spent)
		insertCoin(t, repo, "bb:0", ids["present coin"], delegation.Present)
		insertCoin(t, repo, "cc:0", ids["just spent"], delegation.Spent)
		_, err = repo.db.db.ExecContext(
			t.Context(),
			"UPDATE coins SET updated_at = now() - interval '2 hours' WHERE outpoint <> 'cc:0'",
		)
		require.NoError(t, err)

		expired, err := repo.ExpireSettled(t.Context(), time.Now().Add(-time.Hour), 10)
		require.NoError(t, err)
		require.ElementsMatch(t, []int64{ids["spent"], ids["never funded"]}, listIDs(expired))
		for name, status := range map[string]string{
			"spent": delegation.StatusExpired, "never funded": delegation.StatusExpired,
			"present coin": delegation.StatusActive, "just spent": delegation.StatusActive,
			"recent": delegation.StatusActive, "cancelled": delegation.StatusCancelled,
			"client": delegation.StatusActive,
		} {
			got, err := repo.Get(t.Context(), ids[name])
			require.NoError(t, err)
			require.Equal(t, status, got.Status, name)
		}
	})
	t.Run("at most limit", func(t *testing.T) {
		repo := testDelegations(t)
		stream := testStream()
		require.NoError(t, repo.db.Streams().AddOrUpdate(t.Context(), &stream))
		var ids []int64
		for _, fp := range []string{"a", "b", "c"} {
			created := testSpawned(fp, stream.ID)
			require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
			backdate(t, repo, "updated_at", created.ID, 2*time.Hour)
			ids = append(ids, created.ID)
		}

		first, err := repo.ExpireSettled(t.Context(), time.Now().Add(-time.Hour), 2)
		require.NoError(t, err)
		require.Len(t, first, 2)
		rest, err := repo.ExpireSettled(t.Context(), time.Now().Add(-time.Hour), 2)
		require.NoError(t, err)
		require.ElementsMatch(t, ids, listIDs(append(first, rest...)), "the rest on the next call")
	})
}

func TestDelegationPruneIdle(t *testing.T) {
	repo := testDelegations(t)
	stream := testStream()
	require.NoError(t, repo.db.Streams().AddOrUpdate(t.Context(), &stream))
	ids := map[string]int64{}
	for _, name := range []string{"idle", "coin", "attempt", "recent", "active", "cancelled"} {
		created := testDelegation(name)
		require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
		ids[name] = created.ID
	}
	spawned := testSpawned("spawned", stream.ID)
	require.NoError(t, repo.AddOrUpdate(t.Context(), &spawned))
	ids["spawned"] = spawned.ID
	for name, id := range ids {
		status := delegation.StatusExpired
		switch name {
		case "active":
			continue
		case "cancelled":
			status = delegation.StatusCancelled
		}
		require.NoError(
			t, repo.AddOrUpdate(t.Context(), &delegation.Delegation{ID: id, Status: status}),
		)
		if name != "recent" {
			backdate(t, repo, "updated_at", id, 48*time.Hour)
		}
	}
	insertCoin(t, repo, "aa:0", ids["coin"], delegation.Spent)
	require.NoError(t, repo.db.Attempts().AddOrUpdate(t.Context(), &delegation.Attempt{
		DelegationID: ids["attempt"], Coins: []string{"moved:0"}, Status: delegation.Failed,
	}), "an attempt whose coins moved to a successor")

	n, err := repo.PruneIdle(t.Context(), time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
	_, err = repo.Get(t.Context(), ids["idle"])
	require.ErrorIs(t, err, delegation.ErrDelegationNotFound)
	for _, name := range []string{"coin", "attempt", "recent", "active", "cancelled", "spawned"} {
		_, err = repo.Get(t.Context(), ids[name])
		require.NoError(t, err, name)
	}
}

func TestDelegationListAndCount(t *testing.T) {
	repo := testDelegations(t)
	var ids []int64
	for _, fp := range []string{"a", "b", "c", "d", "e"} {
		created := testDelegation(fp)
		require.NoError(t, repo.AddOrUpdate(t.Context(), &created))
		ids = append(ids, created.ID)
	}
	second, err := repo.Get(t.Context(), ids[1])
	require.NoError(t, err)
	require.NoError(t, second.Cancel())
	require.NoError(t, repo.AddOrUpdate(t.Context(), second))

	all, err := repo.List(t.Context(), delegation.ListFilter{Limit: 10})
	require.NoError(t, err)
	require.Equal(t, []int64{ids[4], ids[3], ids[2], ids[1], ids[0]}, listIDs(all))
	require.Equal(t, testDelegation("e").Inputs, all[0].Inputs)

	active, err := repo.List(
		t.Context(), delegation.ListFilter{Status: delegation.StatusActive, Limit: 10},
	)
	require.NoError(t, err)
	require.Equal(t, []int64{ids[4], ids[3], ids[2], ids[0]}, listIDs(active))
	cancelled, err := repo.List(
		t.Context(), delegation.ListFilter{Status: delegation.StatusCancelled, Limit: 10},
	)
	require.NoError(t, err)
	require.Equal(t, []int64{ids[1]}, listIDs(cancelled))

	page, err := repo.List(t.Context(), delegation.ListFilter{Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []int64{ids[4], ids[3]}, listIDs(page))
	page, err = repo.List(t.Context(), delegation.ListFilter{Cursor: page[1].ID, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []int64{ids[2], ids[1]}, listIDs(page))
	page, err = repo.List(t.Context(), delegation.ListFilter{Cursor: page[1].ID, Limit: 2})
	require.NoError(t, err)
	require.Equal(t, []int64{ids[0]}, listIDs(page))
	page, err = repo.List(t.Context(), delegation.ListFilter{Cursor: page[0].ID, Limit: 2})
	require.NoError(t, err)
	require.Empty(t, page)

	clients, spawned, err := repo.CountActive(t.Context())
	require.NoError(t, err)
	require.Equal(t, int64(4), clients)
	require.Zero(t, spawned)
}

func testDelegations(t *testing.T) *DelegationRepository {
	t.Helper()
	db := testDB(t)
	tpl := testTemplate("t")
	require.NoError(t, db.Templates().AddOrUpdate(t.Context(), &tpl))
	return db.Delegations()
}

func testDelegation(fingerprint string) delegation.Delegation {
	return delegation.Delegation{
		Fingerprint: fingerprint,
		TemplateID:  "t",
		Variables:   map[string]string{"owner": "02ab"},
		Inputs: []delegation.Input{{
			Name: "funds", Script: "5120aa", Tapscripts: []string{"51"},
			Address: "addr-" + fingerprint, Sequence: 0xffffffff,
		}},
		Status: delegation.StatusActive,
	}
}

func testSpawned(fingerprint string, streamID int64) delegation.Delegation {
	d := testDelegation(fingerprint)
	d.StreamID = streamID
	return d
}

func testStream() delegation.Stream {
	return delegation.Stream{
		TemplateID: "t",
		Variables:  map[string]string{"a": "01"},
		Status:     delegation.StatusActive,
	}
}

func withExpiry(d delegation.Delegation, at *time.Time) delegation.Delegation {
	d.ExpiresAt = at
	return d
}

func backdate(
	t *testing.T, repo *DelegationRepository, column string, id int64, age time.Duration,
) {
	t.Helper()
	_, err := repo.db.db.ExecContext(
		t.Context(),
		"UPDATE delegations SET "+column+" = now() - make_interval(secs => $1) WHERE id = $2",
		age.Seconds(), id,
	)
	require.NoError(t, err)
}

func insertCoin(
	t *testing.T, repo *DelegationRepository, outpoint string, id int64,
	status delegation.CoinStatus,
) {
	t.Helper()
	_, err := repo.db.db.ExecContext(
		t.Context(), "INSERT INTO coins (outpoint, delegation_id, input, script, amount, "+
			"created_at, status) VALUES ($1, $2, 0, 's', 1, now(), $3)", outpoint, id, status,
	)
	require.NoError(t, err)
}

func listIDs(list []delegation.Delegation) []int64 {
	ids := make([]int64, 0, len(list))
	for _, d := range list {
		ids = append(ids, d.ID)
	}
	return ids
}
