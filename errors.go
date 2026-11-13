package wazero

import "github.com/tetratelabs/wazero/internal/wasmruntime"

// ErrRuntimeMemoryQuotaExceeded is returned when instantiating a module would
// exceed the total memory quota configured via
// RuntimeConfig.WithTotalMemoryLimitPages. The failed instantiation is fully
// rolled back, so the quota is left unchanged and remains reusable.
var ErrRuntimeMemoryQuotaExceeded = wasmruntime.ErrRuntimeMemoryQuotaExceeded
