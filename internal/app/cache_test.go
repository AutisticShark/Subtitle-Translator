package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRedis speaks enough RESP2 for go-redis GET/SET; HELLO is refused so the
// client falls back to RESP2. It is a stand-in when TEST_REDIS_URL is not set.
type fakeRedis struct {
	listener net.Listener
	mu       sync.Mutex
	values   map[string]string
	gets     int
	sets     int
}

func startFakeRedis(t *testing.T) *fakeRedis {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeRedis{listener: listener, values: map[string]string{}}
	go func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(connection)
		}
	}()
	t.Cleanup(func() { listener.Close() })
	return server
}

func (f *fakeRedis) url() string { return "redis://" + f.listener.Addr().String() + "/0" }

func (f *fakeRedis) serve(connection net.Conn) {
	defer connection.Close()
	reader := bufio.NewReader(connection)
	for {
		args, err := readCommand(reader)
		if err != nil {
			return
		}
		f.mu.Lock()
		var reply string
		switch strings.ToUpper(args[0]) {
		case "GET":
			f.gets++
			if value, ok := f.values[args[1]]; ok {
				reply = "$" + strconv.Itoa(len(value)) + "\r\n" + value + "\r\n"
			} else {
				reply = "$-1\r\n"
			}
		case "SET":
			f.sets++
			f.values[args[1]] = args[2]
			reply = "+OK\r\n"
		case "PING", "SELECT":
			reply = "+OK\r\n"
		default:
			reply = "-ERR unknown command\r\n"
		}
		f.mu.Unlock()
		if _, err := io.WriteString(connection, reply); err != nil {
			return
		}
	}
}

func readCommand(reader *bufio.Reader) ([]string, error) {
	line, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(line, "*") {
		return nil, fmt.Errorf("unexpected %q", line)
	}
	count, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	args := make([]string, count)
	for i := range args {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		size, _ := strconv.Atoi(strings.TrimSpace(header[1:]))
		buffer := make([]byte, size+2)
		if _, err := io.ReadFull(reader, buffer); err != nil {
			return nil, err
		}
		args[i] = string(buffer[:size])
	}
	return args, nil
}

func TestRedisCacheIsEncryptedRevisionedAndOptional(t *testing.T) {
	redisURL := os.Getenv("TEST_REDIS_URL")
	var fake *fakeRedis
	if redisURL == "" {
		fake = startFakeRedis(t)
		redisURL = fake.url()
	}
	env := newEnv(t, func(c *Config) { c.RedisURL = redisURL; c.RedisKeyPrefix = "st-test-" + randomHex(4) })
	admin := env.setupAdmin()
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{
		"openai_api_key": "sk-cached-secret", "rpm": "7",
	}), http.StatusOK)
	for range 2 {
		if admin.get("/api/settings").json()["rpm"] != "7" {
			t.Fatal("settings not read through the cache")
		}
	}
	if fake != nil {
		fake.mu.Lock()
		for key, value := range fake.values {
			if strings.Contains(value, "sk-cached-secret") || strings.Contains(value, "rpm") {
				t.Fatalf("cache entry %s stored in plaintext", key)
			}
		}
		if fake.sets == 0 || fake.gets == 0 {
			t.Fatalf("cache unused: %d gets, %d sets", fake.gets, fake.sets)
		}
		fake.mu.Unlock()
	}
	// A write changes the revision, so the next read is fresh, not cached.
	expectStatus(t, admin.send(http.MethodPut, "/api/settings", map[string]any{"rpm": "9"}), http.StatusOK)
	if admin.get("/api/settings").json()["rpm"] != "9" {
		t.Fatal("stale settings served after a write")
	}
	// Job polling sees progress through the cache.
	id := jobIDs(t, admin.upload(map[string]string{"provider": "echo", "target_languages": "ja"},
		map[string]string{"a.srt": testSubtitle}))[0]
	if job := env.waitForJob(admin, id); job["status"] != "completed" {
		t.Fatalf("job %v", job)
	}
	if list := admin.get("/api/jobs").json(); list["counts"].(map[string]any)["completed"] != float64(1) {
		t.Fatalf("stale job counts %v", list["counts"])
	}
	// An entry moved to another key does not decrypt as valid there.
	if fake != nil {
		ctx := context.Background()
		value, err := remember(ctx, env.server.cache, "probe:a", func() (string, error) { return "secret-a", nil })
		if err != nil || value != "secret-a" {
			t.Fatal(err)
		}
		fake.mu.Lock()
		fake.values[env.server.cache.prefix+":probe:b"] = fake.values[env.server.cache.prefix+":probe:a"]
		fake.mu.Unlock()
		value, _ = remember(ctx, env.server.cache, "probe:b", func() (string, error) { return "fresh-b", nil })
		if value != "fresh-b" {
			t.Fatalf("moved entry accepted: %q", value)
		}
		// An outage falls back to SQL.
		fake.listener.Close()
		start := time.Now()
		expectStatus(t, admin.get("/api/settings"), http.StatusOK)
		expectStatus(t, admin.get("/api/jobs"), http.StatusOK)
		if time.Since(start) > 3*time.Second {
			t.Fatal("outage slowed requests down")
		}
	}
}

func TestInvalidRedisURLIsRejectedWithoutEchoingIt(t *testing.T) {
	_, err := newReadCache("http://user:hunter2@host", 60, "x", "db", nil, nil, time.Now)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error %v", err)
	}
}
