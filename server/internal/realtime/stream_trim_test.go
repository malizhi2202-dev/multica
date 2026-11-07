package realtime

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/go-redis/redismock/v9"
	"github.com/redis/go-redis/v9"
)

func TestTrimStreamBeforeFallsBackOnRedis60(t *testing.T) {
	rdb, mock := redismock.NewClientMock()
	t.Cleanup(func() { _ = rdb.Close() })
	var unsupported atomic.Bool
	const stream = "ws:relay:shard:0"
	const minID = "1700000001000-0"

	mock.ExpectXTrimMinID(stream, minID).SetErr(errors.New("ERR syntax error"))
	mock.ExpectXRangeN(stream, "-", minID, 256).SetVal([]redis.XMessage{
		{ID: "1700000000000-1"}, {ID: minID},
	})
	mock.ExpectXDel(stream, "1700000000000-1").SetVal(1)
	trimmed, err := trimStreamBefore(context.Background(), rdb, &unsupported, stream, minID)
	if err != nil || trimmed != 1 || !unsupported.Load() {
		t.Fatalf("first trim = %d, unsupported=%v, err=%v", trimmed, unsupported.Load(), err)
	}

	mock.ExpectXRangeN(stream, "-", minID, 256).SetVal(nil)
	trimmed, err = trimStreamBefore(context.Background(), rdb, &unsupported, stream, minID)
	if err != nil || trimmed != 0 {
		t.Fatalf("second trim = %d, err=%v", trimmed, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
