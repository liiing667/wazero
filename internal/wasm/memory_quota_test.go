package wasm

import (
	"sync"
	"testing"

	"github.com/tetratelabs/wazero/internal/testing/require"
)

func TestMemoryQuota_Disabled(t *testing.T) {
	// A zero limit disables enforcement, and a nil quota is a no-op.
	for _, q := range []*MemoryQuota{nil, NewMemoryQuota(0)} {
		require.True(t, q.TryReserve(1_000_000))
		require.Equal(t, uint32(0), q.LimitPages())
		q.Release(1_000_000) // must not panic
	}
}

func TestMemoryQuota_ReserveAndRelease(t *testing.T) {
	q := NewMemoryQuota(5)
	require.Equal(t, uint32(5), q.LimitPages())

	require.True(t, q.TryReserve(2))
	require.Equal(t, uint32(2), q.UsedPages())

	// Exactly the remaining quota fits.
	require.True(t, q.TryReserve(3))
	require.Equal(t, uint32(5), q.UsedPages())

	// Anything beyond the limit is rejected without mutating the counter.
	require.False(t, q.TryReserve(1))
	require.Equal(t, uint32(5), q.UsedPages())

	// Releasing makes the quota reusable again.
	q.Release(2)
	require.Equal(t, uint32(3), q.UsedPages())
	require.True(t, q.TryReserve(2))
	require.Equal(t, uint32(5), q.UsedPages())

	// Releasing more than used clamps to zero instead of underflowing.
	q.Release(99)
	require.Equal(t, uint32(0), q.UsedPages())
}

func TestMemoryQuota_ConcurrentReservations(t *testing.T) {
	const limit = 64
	q := NewMemoryQuota(limit)

	var wg sync.WaitGroup
	success := make(chan bool, limit*2)
	// Each goroutine repeatedly reserves a single page until the quota is
	// exhausted; exactly "limit" reservations must succeed in total.
	for i := 0; i < limit*2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			success <- q.TryReserve(1)
		}()
	}
	wg.Wait()
	close(success)

	granted := 0
	for ok := range success {
		if ok {
			granted++
		}
	}
	require.Equal(t, limit, granted)
	require.Equal(t, uint32(limit), q.UsedPages())
}
