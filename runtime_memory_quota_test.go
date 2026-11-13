package wazero

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tetratelabs/wazero/experimental"
	"github.com/tetratelabs/wazero/internal/platform"
	"github.com/tetratelabs/wazero/internal/testing/binaryencoding"
	"github.com/tetratelabs/wazero/internal/testing/require"
	"github.com/tetratelabs/wazero/internal/wasm"
)

// newMemoryQuotaWasm builds a small module whose linear memory starts at one
// page and may grow up to maxPages. It exports the memory plus two functions:
// "size" returns the current page count, "grow" grows by the i32 argument.
func newMemoryQuotaWasm(maxPages uint32) []byte {
	return binaryencoding.EncodeModule(&wasm.Module{
		TypeSection: []wasm.FunctionType{
			{}, // index 0: () -> ()
			{Results: []wasm.ValueType{wasm.ValueTypeI32}},                                              // index 1: () -> i32
			{Params: []wasm.ValueType{wasm.ValueTypeI32}, Results: []wasm.ValueType{wasm.ValueTypeI32}}, // index 2: (i32) -> i32
		},
		FunctionSection: []wasm.Index{1, 2},
		MemorySection:   &wasm.Memory{Min: 1, Cap: maxPages, Max: maxPages, IsMaxEncoded: true},
		ExportSection: []wasm.Export{
			{Name: "memory", Type: wasm.ExternTypeMemory, Index: 0},
			{Name: "size", Type: wasm.ExternTypeFunc, Index: 0},
			{Name: "grow", Type: wasm.ExternTypeFunc, Index: 1},
		},
		CodeSection: []wasm.Code{
			{Body: []byte{wasm.OpcodeMemorySize, 0, wasm.OpcodeEnd}},
			{Body: []byte{wasm.OpcodeLocalGet, 0, wasm.OpcodeMemoryGrow, 0, wasm.OpcodeEnd}},
		},
	})
}

func TestRuntime_TotalMemoryLimitPages(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config RuntimeConfig
	}{
		{name: "compiler", config: NewRuntimeConfigCompiler()},
		{name: "interpreter", config: NewRuntimeConfigInterpreter()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "compiler" && !platform.CompilerSupported() {
				t.Skip("compiler is not supported on this host")
			}

			ctx := context.Background()
			binA := newMemoryQuotaWasm(4)
			binB := newMemoryQuotaWasm(4)

			t.Run("shared across modules with rollback and reuse", func(t *testing.T) {
				// Three-page total quota: each one-page module shares the
				// same counter, so at most three can be live at once.
				r := NewRuntimeWithConfig(ctx, tc.config.WithTotalMemoryLimitPages(3))
				defer r.Close(ctx)

				modA, err := r.InstantiateWithConfig(ctx, binA, NewModuleConfig().WithName("a"))
				require.NoError(t, err)
				modB, err := r.InstantiateWithConfig(ctx, binB, NewModuleConfig().WithName("b"))
				require.NoError(t, err)
				modC, err := r.InstantiateWithConfig(ctx, binA, NewModuleConfig().WithName("c"))
				require.NoError(t, err)

				// A fourth module would exceed the quota and must be rejected
				// with the sentinel error and fully rolled back.
				_, err = r.InstantiateWithConfig(ctx, binA, NewModuleConfig().WithName("overflow"))
				require.Error(t, err)
				require.True(t, errors.Is(err, ErrRuntimeMemoryQuotaExceeded))
				require.Nil(t, r.Module("overflow"))
				require.NotNil(t, r.Module("c")) // existing modules stay intact

				// The failed attempt left the quota untouched, and since the
				// quota is full, a memory.grow is refused as well.
				growA := modA.ExportedFunction("grow")
				var res []uint64
				var size []uint64
				res, err = growA.Call(ctx, 1)
				require.NoError(t, err)
				require.Equal(t, uint64(0xffffffff), res[0])
				size, err = modA.ExportedFunction("size").Call(ctx)
				require.NoError(t, err)
				require.Equal(t, uint64(1), size[0])

				// Closing B returns its page, making the quota reusable for a
				// grow on A.
				require.NoError(t, modB.Close(ctx))
				res, err = growA.Call(ctx, 1)
				require.NoError(t, err)
				require.Equal(t, uint64(1), res[0]) // previous size in pages
				size, err = modA.ExportedFunction("size").Call(ctx)
				require.NoError(t, err)
				require.Equal(t, uint64(2), size[0])

				// Quota state is now A=2, C=1 (total 3); closing C frees a
				// page so a fresh one-page module can take its slot.
				require.NoError(t, modC.Close(ctx))
				modB2, err := r.InstantiateWithConfig(ctx, binB, NewModuleConfig().WithName("b2"))
				require.NoError(t, err)
				require.NotNil(t, modB2)

				// Quota is full again (A=2, b2=1), so the next grow fails.
				res, err = growA.Call(ctx, 1)
				require.NoError(t, err)
				require.Equal(t, uint64(0xffffffff), res[0])
			})

			t.Run("rollback releases quota on start failure", func(t *testing.T) {
				r := NewRuntimeWithConfig(ctx, tc.config.WithTotalMemoryLimitPages(2))
				defer r.Close(ctx)

				// One page used, one remaining.
				_, err := r.InstantiateWithConfig(ctx, binA, NewModuleConfig().WithName("a"))
				require.NoError(t, err)

				// A module whose start function traps must release its
				// reservation on the way out, so retrying afterwards succeeds.
				badStart := binaryencoding.EncodeModule(&wasm.Module{
					TypeSection:     []wasm.FunctionType{{}},
					FunctionSection: []wasm.Index{0},
					MemorySection:   &wasm.Memory{Min: 1, Cap: 1, Max: 1, IsMaxEncoded: true},
					CodeSection:     []wasm.Code{{Body: []byte{wasm.OpcodeUnreachable, wasm.OpcodeEnd}}},
					StartSection:    func() *wasm.Index { i := wasm.Index(0); return &i }(),
				})
				_, err = r.InstantiateWithConfig(ctx, badStart, NewModuleConfig().WithName("bad"))
				require.Error(t, err)

				// Quota was handed back: a well-behaved module fits again.
				modRetry, err := r.InstantiateWithConfig(ctx, binB, NewModuleConfig().WithName("retry"))
				require.NoError(t, err)
				require.Equal(t, wasm.MemoryPageSize, modRetry.Memory().Size())
			})

			t.Run("disabled by default keeps legacy behavior", func(t *testing.T) {
				// Without a total limit, three one-page modules coexist even
				// though the per-memory limit still applies.
				r := NewRuntimeWithConfig(ctx, tc.config)
				defer r.Close(ctx)

				for _, name := range []string{"a", "b", "c"} {
					_, err := r.InstantiateWithConfig(ctx, binA, NewModuleConfig().WithName(name))
					require.NoError(t, err)
				}
			})

			t.Run("imported memory is counted once by its owner", func(t *testing.T) {
				r := NewRuntimeWithConfig(ctx, tc.config.WithTotalMemoryLimitPages(1))
				defer r.Close(ctx)

				owner, err := r.InstantiateWithConfig(ctx, binA, NewModuleConfig().WithName("owner"))
				require.NoError(t, err)

				importer := binaryencoding.EncodeModule(&wasm.Module{
					ImportSection: []wasm.Import{
						{Module: "owner", Name: "memory", Type: wasm.ExternTypeMemory, DescMem: &wasm.Memory{Max: 4}},
					},
					ImportPerModule: map[string][]*wasm.Import{
						"owner": {{Module: "owner", Name: "memory", Type: wasm.ExternTypeMemory, DescMem: &wasm.Memory{Max: 4}}},
					},
				})
				// Importing an existing memory consumes no additional quota,
				// so this succeeds despite the one-page total limit.
				_, err = r.InstantiateWithConfig(ctx, importer, NewModuleConfig().WithName("importer"))
				require.NoError(t, err)

				// And a module that would define its own new page is rejected.
				_, err = r.InstantiateWithConfig(ctx, binB, NewModuleConfig().WithName("extra"))
				require.True(t, errors.Is(err, ErrRuntimeMemoryQuotaExceeded))

				_ = owner
			})

			t.Run("concurrent instantiations never exceed the quota", func(t *testing.T) {
				const quotaPages = 4
				r := NewRuntimeWithConfig(ctx, tc.config.WithTotalMemoryLimitPages(quotaPages))
				defer r.Close(ctx)

				var wg sync.WaitGroup
				start := make(chan struct{})
				results := make(chan error, quotaPages*4)
				// Twice as many goroutines as available pages: only quotaPages
				// of the one-page instantiations may succeed.
				for i := 0; i < quotaPages*2; i++ {
					wg.Add(1)
					go func(i int) {
						defer wg.Done()
						<-start
						cfg := NewModuleConfig().WithName("mod" + string(rune('a'+i)))
						_, err := r.InstantiateWithConfig(ctx, binA, cfg)
						results <- err
					}(i)
				}
				close(start)
				wg.Wait()
				close(results)

				granted, rejected := 0, 0
				for e := range results {
					if e == nil {
						granted++
					} else {
						require.True(t, errors.Is(e, ErrRuntimeMemoryQuotaExceeded))
						rejected++
					}
				}
				require.Equal(t, quotaPages, granted)
				require.Equal(t, quotaPages, rejected)
			})

			t.Run("works alongside an experimental MemoryAllocator", func(t *testing.T) {
				// The custom allocator decides how backing bytes are managed,
				// while the quota still governs how many pages are permitted:
				// the two mechanisms must compose without interfering.
				r := NewRuntimeWithConfig(ctx, tc.config.WithTotalMemoryLimitPages(1))
				defer r.Close(ctx)

				allocator := experimental.MemoryAllocatorFunc(func(cap, max uint64) experimental.LinearMemory {
					return &trackingLinearMemory{buf: make([]byte, 0, cap)}
				})
				allocCtx := experimental.WithMemoryAllocator(ctx, allocator)

				mod, err := r.InstantiateWithConfig(allocCtx, binA, NewModuleConfig().WithName("a"))
				require.NoError(t, err)

				// Quota is consumed even though a custom allocator is in use.
				_, err = r.InstantiateWithConfig(allocCtx, binB, NewModuleConfig().WithName("b"))
				require.True(t, errors.Is(err, ErrRuntimeMemoryQuotaExceeded))

				// Grow beyond the quota is rejected, then succeeds after close.
				grow := mod.ExportedFunction("grow")
				res, callErr := grow.Call(allocCtx, 1)
				require.NoError(t, callErr)
				require.Equal(t, uint64(0xffffffff), res[0])
				require.NoError(t, mod.Close(allocCtx))

				mod2, err := r.InstantiateWithConfig(allocCtx, binA, NewModuleConfig().WithName("a2"))
				require.NoError(t, err)
				require.NotNil(t, mod2)
			})
		})
	}
}

// trackingLinearMemory is a minimal experimental.LinearMemory backed by a
// plain slice, used to verify quota composition with custom allocators.
type trackingLinearMemory struct {
	buf []byte
}

func (m *trackingLinearMemory) Reallocate(size uint64) []byte {
	if uint64(cap(m.buf)) < size {
		grown := make([]byte, size)
		copy(grown, m.buf)
		m.buf = grown
	}
	m.buf = m.buf[:size]
	return m.buf
}

func (m *trackingLinearMemory) Free() {}
