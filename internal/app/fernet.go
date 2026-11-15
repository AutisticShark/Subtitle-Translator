package app

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"strings"
	"time"
)

// fernet implements the Fernet token format (cryptography.fernet), which the
// Python application used for saved secrets ("enc:v1:" values) and for Redis
// cache entries. Tokens written by either implementation decrypt in the other.
type fernet struct {
	signingKey    []byte
	encryptionKey []byte
}

const fernetMaxClockSkew = 60 * time.Second

var errInvalidFernetToken = errors.New("invalid Fernet token")

// newFernet accepts a URL-safe base64 encoding of 32 key bytes.
func newFernet(key string) (*fernet, error) {
	raw, err := decodeURLBase64(key)
	if err != nil || len(raw) != 32 {
		return nil, errors.New("Fernet key must be 32 url-safe base64-encoded bytes")
	}
	return &fernet{signingKey: raw[:16], encryptionKey: raw[16:]}, nil
}

func decodeURLBase64(value string) ([]byte, error) {
	value = strings.TrimSpace(value)
	if decoded, err := base64.URLEncoding.DecodeString(value); err == nil {
		return decoded, nil
	}
	return base64.RawURLEncoding.DecodeString(strings.TrimRight(value, "="))
}

func (f *fernet) encrypt(plaintext []byte, now time.Time) string {
	iv := make([]byte, aes.BlockSize)
	if _, err := rand.Read(iv); err != nil {
		panic(err)
	}
	padding := aes.BlockSize - len(plaintext)%aes.BlockSize
	padded := make([]byte, len(plaintext)+padding)
	copy(padded, plaintext)
	for i := len(plaintext); i < len(padded); i++ {
		padded[i] = byte(padding)
	}
	block, err := aes.NewCipher(f.encryptionKey)
	if err != nil {
		panic(err)
	}
	token := make([]byte, 0, 1+8+aes.BlockSize+len(padded)+sha256.Size)
	token = append(token, 0x80)
	token = binary.BigEndian.AppendUint64(token, uint64(now.Unix()))
	token = append(token, iv...)
	ciphertext := make([]byte, len(padded))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, padded)
	token = append(token, ciphertext...)
	mac := hmac.New(sha256.New, f.signingKey)
	mac.Write(token)
	token = mac.Sum(token)
	return base64.URLEncoding.EncodeToString(token)
}

// decrypt verifies and opens a token. A positive ttl also rejects tokens older
// than ttl or more than a minute in the future, as cryptography does.
func (f *fernet) decrypt(token string, ttl time.Duration, now time.Time) ([]byte, error) {
	raw, err := decodeURLBase64(token)
	if err != nil || len(raw) < 1+8+aes.BlockSize+aes.BlockSize+sha256.Size || raw[0] != 0x80 {
		return nil, errInvalidFernetToken
	}
	body, signature := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, f.signingKey)
	mac.Write(body)
	if subtle.ConstantTimeCompare(mac.Sum(nil), signature) != 1 {
		return nil, errInvalidFernetToken
	}
	if ttl > 0 {
		issued := time.Unix(int64(binary.BigEndian.Uint64(body[1:9])), 0)
		if issued.Add(ttl).Before(now) || now.Add(fernetMaxClockSkew).Before(issued) {
			return nil, errInvalidFernetToken
		}
	}
	iv := body[9 : 9+aes.BlockSize]
	ciphertext := body[9+aes.BlockSize:]
	if len(ciphertext)%aes.BlockSize != 0 {
		return nil, errInvalidFernetToken
	}
	block, err := aes.NewCipher(f.encryptionKey)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)
	padding := int(plain[len(plain)-1])
	if padding < 1 || padding > aes.BlockSize {
		return nil, errInvalidFernetToken
	}
	for _, value := range plain[len(plain)-padding:] {
		if int(value) != padding {
			return nil, errInvalidFernetToken
		}
	}
	return plain[:len(plain)-padding], nil
}

// encryptionKey mirrors webapp.encryption_key: an explicit Fernet key, or one
// derived from the JWT secret.
func encryptionKey(explicit, jwtSecret string) (string, error) {
	if explicit != "" {
		if _, err := newFernet(explicit); err != nil {
			return "", errors.New("API_KEY_ENCRYPTION_KEY must be a valid Fernet key")
		}
		return explicit, nil
	}
	digest := sha256.Sum256([]byte("subtitle-api-keys\x00" + jwtSecret))
	return base64.URLEncoding.EncodeToString(digest[:]), nil
}

const secretPrefix = "enc:v1:"

var errUnreadableSecret = errors.New(
	"A saved secret cannot be decrypted; restore the configured JWT/encryption key")

func (s *Server) encryptSecret(value string) string {
	return secretPrefix + s.cipher.encrypt([]byte(value), s.now())
}

// decryptSecret returns legacy plaintext values unchanged.
func (s *Server) decryptSecret(value string) (string, error) {
	if !strings.HasPrefix(value, secretPrefix) {
		return value, nil
	}
	plain, err := s.cipher.decrypt(value[len(secretPrefix):], 0, s.now())
	if err != nil || !utf8Valid(plain) {
		return "", errUnreadableSecret
	}
	return string(plain), nil
}
