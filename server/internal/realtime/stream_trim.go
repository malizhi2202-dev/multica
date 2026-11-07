package realtime

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/redis/go-redis/v9"
)

// Redis 6.0 has Streams but not XTRIM MINID (added in 6.2). Keep the age
// horizon on older servers by deleting a bounded batch of expired entries.
func trimStreamBefore(ctx context.Context, rdb redis.UniversalClient, minIDUnsupported *atomic.Bool, stream, minID string) (int64, error) {
	if !minIDUnsupported.Load() {
		trimmed, err := rdb.XTrimMinID(ctx, stream, minID).Result()
		if err == nil {
			return trimmed, nil
		}
		message := strings.ToLower(err.Error())
		if !strings.Contains(message, "syntax error") && !strings.Contains(message, "unknown subcommand") {
			return 0, err
		}
		minIDUnsupported.Store(true)
	}

	// XRangeN's end ID is inclusive. Retain an entry exactly at minID.
	const batchSize int64 = 256
	entries, err := rdb.XRangeN(ctx, stream, "-", minID, batchSize).Result()
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.ID != minID {
			ids = append(ids, entry.ID)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}
	return rdb.XDel(ctx, stream, ids...).Result()
}
