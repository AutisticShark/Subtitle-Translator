package app

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters of argon2-cffi's PasswordHasher defaults, which created
// every stored hash. Hashes with other parameters still verify and are
// re-hashed with these after a successful sign-in.
const (
	argonTime    = 3
	argonMemory  = 65536
	argonThreads = 4
	argonKeyLen  = 32
	argonSaltLen = 16
)

var errPasswordMismatch = errors.New("password does not match")

type argonParams struct {
	variant string
	version int
	memory  uint32
	time    uint32
	threads uint8
	salt    []byte
	hash    []byte
}

func hashPassword(password string) string {
	salt := make([]byte, argonSaltLen)
	if _, err := rand.Read(salt); err != nil {
		panic(err)
	}
	key := argon2.IDKey([]byte(password), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory,
		argonTime, argonThreads, base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key))
}

func parseArgonHash(encoded string) (argonParams, error) {
	invalid := errors.New("invalid password hash")
	parts := strings.Split(encoded, "$")
	// "", variant, v=19, m=..,t=..,p=.., salt, hash
	if len(parts) != 6 || parts[0] != "" {
		return argonParams{}, invalid
	}
	params := argonParams{variant: parts[1]}
	if params.variant != "argon2id" && params.variant != "argon2i" {
		return argonParams{}, invalid
	}
	version, ok := strings.CutPrefix(parts[2], "v=")
	if !ok {
		return argonParams{}, invalid
	}
	var err error
	if params.version, err = strconv.Atoi(version); err != nil || params.version != argon2.Version {
		return argonParams{}, invalid
	}
	seen := map[string]bool{}
	for _, field := range strings.Split(parts[3], ",") {
		name, raw, ok := strings.Cut(field, "=")
		value, convErr := strconv.ParseUint(raw, 10, 32)
		if !ok || convErr != nil || seen[name] {
			return argonParams{}, invalid
		}
		seen[name] = true
		switch name {
		case "m":
			params.memory = uint32(value)
		case "t":
			params.time = uint32(value)
		case "p":
			if value == 0 || value > 255 {
				return argonParams{}, invalid
			}
			params.threads = uint8(value)
		default:
			return argonParams{}, invalid
		}
	}
	if !seen["m"] || !seen["t"] || !seen["p"] || params.time == 0 ||
		params.memory < 8*uint32(params.threads) || params.memory > 4*1024*1024 || params.time > 64 {
		return argonParams{}, invalid
	}
	if params.salt, err = base64.RawStdEncoding.DecodeString(parts[4]); err != nil || len(params.salt) < 8 {
		return argonParams{}, invalid
	}
	if params.hash, err = base64.RawStdEncoding.DecodeString(parts[5]); err != nil ||
		len(params.hash) < 4 || len(params.hash) > 1024 {
		return argonParams{}, invalid
	}
	return params, nil
}

// verifyPassword checks a PHC-encoded Argon2 hash.
func verifyPassword(encoded, password string) error {
	params, err := parseArgonHash(encoded)
	if err != nil {
		return err
	}
	keyLen := uint32(len(params.hash))
	var key []byte
	if params.variant == "argon2id" {
		key = argon2.IDKey([]byte(password), params.salt, params.time, params.memory, params.threads, keyLen)
	} else {
		key = argon2.Key([]byte(password), params.salt, params.time, params.memory, params.threads, keyLen)
	}
	if subtle.ConstantTimeCompare(key, params.hash) != 1 {
		return errPasswordMismatch
	}
	return nil
}

// passwordNeedsRehash mirrors PasswordHasher.check_needs_rehash.
func passwordNeedsRehash(encoded string) bool {
	params, err := parseArgonHash(encoded)
	if err != nil {
		return true
	}
	return params.variant != "argon2id" || params.memory != argonMemory || params.time != argonTime ||
		params.threads != argonThreads || len(params.hash) != argonKeyLen || len(params.salt) != argonSaltLen
}
