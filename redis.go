package redsync

import "github.com/go-redis/redis/v7"

// A Pool maintains a pool of Redis connections.
type Pool interface {
	Get() redis.Conn
}
