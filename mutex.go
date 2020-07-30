package redsync

import (
	"crypto/rand"
	"encoding/base64"
	"time"

	"github.com/go-redis/redis/v7"
	"github.com/hashicorp/go-multierror"
)

// A DelayFunc is used to decide the amount of time to wait between retries.
type DelayFunc func(tries int) time.Duration

// A Mutex is a distributed mutual exclusion lock.
type Mutex struct {
	name   string
	expiry time.Duration

	tries     int
	delayFunc DelayFunc

	factor float64

	quorum int

	genValueFunc func() (string, error)
	value        string
	until        time.Time

	pools []Pool
}

// Lock locks m. In case it returns an error on failure, you may retry to acquire the lock by calling this method again.
func (m *Mutex) Lock() error {
	value, err := m.genValueFunc()
	if err != nil {
		return err
	}

	for i := 0; i < m.tries; i++ {
		if i != 0 {
			time.Sleep(m.delayFunc(i))
		}

		start := time.Now()

		n, err := m.actOnPoolsAsync(func(pool Pool) (bool, error) {
			return m.acquire(pool, value)
		})
		if n == 0 && err != nil {
			return err
		}

		now := time.Now()
		newValidityTime := m.expiry - now.Sub(start) - time.Duration(int64(float64(m.expiry)*m.factor))
		if n >= m.quorum && newValidityTime > 0 {
			m.value = value
			m.until = now.Add(newValidityTime)
			return nil
		}
		m.actOnPoolsAsync(func(pool Pool) (bool, error) {
			return m.release(pool, value)
		})
	}

	return ErrFailed
}

// Unlock unlocks m and returns the status of unlock.
func (m *Mutex) Unlock() (bool, error) {
	n, err := m.actOnPoolsAsync(func(pool Pool) (bool, error) {
		return m.release(pool, m.value)
	})
	if n < m.quorum {
		return false, err
	}
	return true, nil
}

// Extend resets the mutex's expiry and returns the status of expiry extension.
func (m *Mutex) Extend() (bool, error) {
	n, err := m.actOnPoolsAsync(func(pool Pool) (bool, error) {
		return m.touch(pool, m.value, int(m.expiry/time.Millisecond))
	})
	if n < m.quorum {
		return false, err
	}
	return true, nil
}

func (m *Mutex) Valid() (bool, error) {
	n, err := m.actOnPoolsAsync(func(pool Pool) (bool, error) {
		return m.valid(pool)
	})
	return n >= m.quorum, err
}

func (m *Mutex) valid(pool Pool) (bool, error) {
	conn := pool.Get()
	defer conn.Close()
	reply, err := conn.Get(m.name).Result()
	if err != nil {
		return false, err
	}
	return m.value == reply, nil
}

func genValue() (string, error) {
	b := make([]byte, 16)
	_, err := rand.Read(b)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

func (m *Mutex) acquire(pool Pool, value string) (bool, error) {
	conn := pool.Get()
	defer conn.Close()
	reply, err := conn.SetNX(m.name, value, m.expiry).Result()
	if err != nil {
		if err == redis.Nil {
			return false, nil
		}
		return false, err
	}
	return reply, nil
}

var deleteScript = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then
		return redis.call("DEL", KEYS[1])
	else
		return 0
	end
`)

func (m *Mutex) release(pool Pool, value string) (bool, error) {
	conn := pool.Get()
	defer conn.Close()
	status, err := deleteScript.Run(conn, []string{m.name}, value).Result()

	return err == nil && status != 0, err
}

var touchScript = redis.NewScript(`
	if redis.call("GET", KEYS[1]) == ARGV[1] then
		return redis.call("pexpire", KEYS[1], ARGV[2])
	else
		return 0
	end
`)

func (m *Mutex) touch(pool Pool, value string, expiry int) (bool, error) {
	conn := pool.Get()
	defer conn.Close()
	status, err := touchScript.Run(conn, []string{m.name}, value, expiry).Result()

	return err == nil && status != 0, err
}

func (m *Mutex) actOnPoolsAsync(actFn func(Pool) (bool, error)) (int, error) {
	type result struct {
		Status bool
		Err    error
	}

	ch := make(chan result)
	for _, pool := range m.pools {
		go func(pool Pool) {
			r := result{}
			r.Status, r.Err = actFn(pool)
			ch <- r
		}(pool)
	}
	n := 0
	var err error
	for range m.pools {
		r := <-ch
		if r.Status {
			n++
		} else if r.Err != nil {
			err = multierror.Append(err, r.Err)
		}
	}
	return n, err
}
