package convtel

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
)

var (
	defaultOnce sync.Once
	defaultExp  atomic.Pointer[Exporter]
)

// Default returns the process-wide exporter configured from the environment,
// or nil when conversation export is disabled or misconfigured. A
// configuration error is reported once on stderr.
func Default() *Exporter {
	defaultOnce.Do(func() {
		cfg, ok, err := ConfigFromEnv(os.Getenv)
		if err != nil {
			fmt.Fprintf(os.Stderr, "gc: conversation export disabled: %v\n", err)
			return
		}
		if !ok {
			return
		}
		res := []Attr{strAttr("service.name", "gascity"), strAttr("gc.signal", "conversation")}
		if v := os.Getenv("GC_CITY"); v != "" {
			res = append(res, strAttr("gc.city", v))
		}
		defaultExp.Store(NewExporter(cfg, res))
	})
	return defaultExp.Load()
}

// ShutdownDefault flushes and closes the process-wide exporter, if started.
func ShutdownDefault(ctx context.Context) error {
	return defaultExp.Load().Close(ctx)
}
