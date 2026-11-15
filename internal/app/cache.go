package app

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// readCache is the optional, disposable Redis cache (redis_cache.py). SQL stays
// authoritative: every read first fetches a revision marker that each write
// changes in its own transaction, and entries are Fernet-encrypted and bound to
// their key, so a value moved to another user's key never decrypts as valid.
type readCache struct {
	client  *redis.Client
	cipher  *fernet
	prefix  string
	ttl     time.Duration
	logger  *slog.Logger
	now     func() time.Time
	mu      sync.Mutex
	retryAt time.Time
}

func newReadCache(rawURL string, ttlSeconds int, namespace, databaseIdentity string, cipher *fernet,
	logger *slog.Logger, now func() time.Time) (*readCache, error) {
	digest := sha256.Sum256([]byte(databaseIdentity))
	cache := &readCache{
		cipher: cipher,
		// "go1" keeps these entries apart from the Python implementation's "v1"
		// entries, whose row encoding differs.
		prefix: namespace + ":go1:" + hex.EncodeToString(digest[:])[:24],
		ttl:    time.Duration(ttlSeconds) * time.Second,
		logger: logger,
		now:    now,
	}
	if rawURL == "" {
		return cache, nil
	}
	options, err := redis.ParseURL(rawURL)
	if err != nil {
		// Never include the URL, which can contain credentials.
		return nil, errors.New("REDIS_URL must be a valid Redis connection URL")
	}
	options.DialTimeout = 250 * time.Millisecond
	options.ReadTimeout = 250 * time.Millisecond
	options.WriteTimeout = 250 * time.Millisecond
	options.MaxRetries = -1
	options.PoolSize = 16
	cache.client = redis.NewClient(options)
	return cache, nil
}

func (c *readCache) available() bool {
	if c == nil || c.client == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return !time.Now().Before(c.retryAt)
}

func (c *readCache) failed() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !time.Now().Before(c.retryAt) {
		c.logger.Warn("Redis cache unavailable; using the database for 5 seconds")
	}
	c.retryAt = time.Now().Add(5 * time.Second)
}

func (c *readCache) close() error {
	if c == nil || c.client == nil {
		return nil
	}
	return c.client.Close()
}

type cacheEnvelope struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

// remember returns the cached value for key or stores what load returns.
func remember[T any](ctx context.Context, c *readCache, key string, load func() (T, error)) (T, error) {
	if !c.available() {
		return load()
	}
	cacheKey := c.prefix + ":" + key
	raw, err := c.client.Get(ctx, cacheKey).Bytes()
	switch {
	case errors.Is(err, redis.Nil):
	case err != nil:
		c.failed()
		return load()
	default:
		if plain, err := c.cipher.decrypt(string(raw), c.ttl, c.now()); err == nil {
			var envelope cacheEnvelope
			if json.Unmarshal(plain, &envelope) == nil && envelope.Key == cacheKey {
				var value T
				if json.Unmarshal(envelope.Value, &value) == nil {
					return value, nil
				}
			}
		}
	}
	value, err := load()
	if err != nil {
		return value, err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return value, nil
	}
	envelope, err := json.Marshal(cacheEnvelope{Key: cacheKey, Value: encoded})
	if err != nil {
		return value, nil
	}
	if err := c.client.Set(ctx, cacheKey, c.cipher.encrypt(envelope, c.now()), c.ttl).Err(); err != nil {
		c.failed()
	}
	return value, nil
}

// cachedQuery reads the SQL revision first, then uses Redis for name:revision:scope.
func cachedQuery[T any](ctx context.Context, s *Server, name, scope string, load func(c conn) (T, error)) (T, error) {
	c := s.db.conn()
	if !s.cache.available() {
		return load(c)
	}
	var revision string
	err := c.queryRow(ctx, "SELECT revision FROM cache_revisions WHERE name = ?", name).Scan(&revision)
	if err != nil {
		return load(c)
	}
	return remember(ctx, s.cache, name+":"+revision+":"+scope, func() (T, error) { return load(c) })
}
