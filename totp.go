package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

const totpPeriod = 30
const totpDigits = 6
const totpSkew = 1 // accept one step either side of now

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newTOTPSecret() string {
	b := make([]byte, 20)
	rand.Read(b)
	return b32.EncodeToString(b)
}

// totpAt computes the RFC 6238 code for a given time step.
func totpAt(secret string, step int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	off := sum[len(sum)-1] & 0x0f
	val := (uint32(sum[off]&0x7f) << 24) | (uint32(sum[off+1]) << 16) |
		(uint32(sum[off+2]) << 8) | uint32(sum[off+3])
	return fmt.Sprintf("%0*d", totpDigits, val%1000000), nil
}

// verifyTOTP checks a code against the current window. It returns the step the
// code belongs to so the caller can reject replays of an already-used code.
func verifyTOTP(secret, code string, now int64, lastStep int64) (int64, bool) {
	code = strings.TrimSpace(strings.ReplaceAll(code, " ", ""))
	if len(code) != totpDigits {
		return 0, false
	}
	cur := now / totpPeriod
	for d := -totpSkew; d <= totpSkew; d++ {
		step := cur + int64(d)
		if step <= lastStep {
			continue // already used, or older than the last accepted code
		}
		want, err := totpAt(secret, step)
		if err != nil {
			return 0, false
		}
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 {
			return step, true
		}
	}
	return 0, false
}

func otpauthURI(issuer, account, secret string) string {
	label := url.PathEscape(issuer + ":" + account)
	v := url.Values{}
	v.Set("secret", secret)
	v.Set("issuer", issuer)
	return "otpauth://totp/" + label + "?" + v.Encode()
}

// groupSecret formats the shared secret for manual entry.
func groupSecret(s string) string {
	var b strings.Builder
	for i, r := range s {
		if i > 0 && i%4 == 0 {
			b.WriteByte(' ')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// ---------- backup codes ----------

const backupCodeCount = 10

var codeAlphabet = []byte("ABCDEFGHJKLMNPQRSTUVWXYZ23456789") // no I/O/0/1

func newBackupCodes() (plain []string, hashed []string) {
	for i := 0; i < backupCodeCount; i++ {
		b := make([]byte, 10)
		rand.Read(b)
		var sb strings.Builder
		for j, x := range b {
			if j == 5 {
				sb.WriteByte('-')
			}
			sb.WriteByte(codeAlphabet[int(x)%len(codeAlphabet)])
		}
		c := sb.String()
		plain = append(plain, c)
		hashed = append(hashed, hashBackupCode(c))
	}
	return
}

func hashBackupCode(c string) string {
	c = strings.ToUpper(strings.TrimSpace(strings.ReplaceAll(c, " ", "")))
	sum := sha256.Sum256([]byte("backup:" + c))
	return hex.EncodeToString(sum[:])
}

// useBackupCode consumes a matching code, returning the remaining set.
func useBackupCode(codes []string, entered string) ([]string, bool) {
	want := hashBackupCode(entered)
	for i, h := range codes {
		if subtle.ConstantTimeCompare([]byte(h), []byte(want)) == 1 {
			return append(append([]string{}, codes[:i]...), codes[i+1:]...), true
		}
	}
	return codes, false
}
