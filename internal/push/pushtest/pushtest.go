// Package pushtest is a browser's side of Web Push for tests: a key pair
// and auth secret to subscribe with, and RFC 8291 decryption of what the
// push service receives. It never sends anything.
package pushtest

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
)

// Device is a browser's push keys.
type Device struct {
	Priv *ecdh.PrivateKey
	Auth []byte
}

// NewDevice makes a fresh P-256 key pair and 16-byte auth secret.
func NewDevice() (*Device, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	auth := make([]byte, 16)
	if _, err := rand.Read(auth); err != nil {
		return nil, err
	}
	return &Device{Priv: priv, Auth: auth}, nil
}

// P256dh is the public key as PushSubscription.toJSON() gives it.
func (d *Device) P256dh() string {
	return base64.RawURLEncoding.EncodeToString(d.Priv.PublicKey().Bytes())
}

// AuthKey is the auth secret as PushSubscription.toJSON() gives it.
func (d *Device) AuthKey() string { return base64.RawURLEncoding.EncodeToString(d.Auth) }

// Decrypt undoes the aes128gcm content coding (RFC 8188 with RFC 8291
// keys): a header (salt, rs, keyid = the sender's public key), then one
// record ending in the 0x02 delimiter and zero padding.
func (d *Device) Decrypt(body []byte) ([]byte, error) {
	if len(body) < 21 {
		return nil, fmt.Errorf("body too short: %d", len(body))
	}
	salt := body[:16]
	rs := binary.BigEndian.Uint32(body[16:20])
	idLen := int(body[20])
	if len(body) < 21+idLen {
		return nil, errors.New("truncated keyid")
	}
	senderPub := body[21 : 21+idLen]
	record := body[21+idLen:]
	if uint32(len(body)) > rs {
		return nil, fmt.Errorf("one record expected: body %d > rs %d", len(body), rs)
	}
	sender, err := ecdh.P256().NewPublicKey(senderPub)
	if err != nil {
		return nil, fmt.Errorf("sender key: %w", err)
	}
	shared, err := d.Priv.ECDH(sender)
	if err != nil {
		return nil, err
	}
	info := "WebPush: info\x00" + string(d.Priv.PublicKey().Bytes()) + string(senderPub)
	ikm, err := hkdf.Key(sha256.New, shared, d.Auth, info, 32)
	if err != nil {
		return nil, err
	}
	cek, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: aes128gcm\x00", 16)
	if err != nil {
		return nil, err
	}
	nonce, err := hkdf.Key(sha256.New, ikm, salt, "Content-Encoding: nonce\x00", 12)
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(cek)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	padded, err := gcm.Open(nil, nonce, record, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	end := bytes.LastIndexFunc(padded, func(r rune) bool { return r != 0 })
	if end < 0 || padded[end] != 0x02 {
		return nil, errors.New("missing last-record delimiter")
	}
	return padded[:end], nil
}
