package idgen

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

var enc = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

func New(prefix string) string {
	b := make([]byte, 10)
	_, _ = rand.Read(b)
	ts := uint64(time.Now().UnixMilli())
	tb := []byte{byte(ts >> 40), byte(ts >> 32), byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts)}
	return prefix + "_" + strings.ToLower(enc.EncodeToString(append(tb, b...)))
}
