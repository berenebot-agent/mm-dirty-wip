package idgen

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
	"time"
)

var enc = base32.NewEncoding("0123456789ABCDEFGHJKMNPQRSTVWXYZ").WithPadding(base32.NoPadding)

// New returns a time-prefixed random identifier. The random suffix is filled
// from the OS CSPRNG; a failure is fatal after a brief retry rather than
// silently continuing with an all-zero/colliding suffix, which would corrupt
// identifier uniqueness. OS RNG failure is effectively unrecoverable anyway.
func New(prefix string) string {
	b := make([]byte, 10)
	if err := fillRandom(b); err != nil {
		panic("idgen: crypto/rand failed: " + err.Error())
	}
	ts := uint64(time.Now().UnixMilli())
	tb := []byte{byte(ts >> 40), byte(ts >> 32), byte(ts >> 24), byte(ts >> 16), byte(ts >> 8), byte(ts)}
	return prefix + "_" + strings.ToLower(enc.EncodeToString(append(tb, b...)))
}

func fillRandom(b []byte) error {
	var err error
	for i := 0; i < 3; i++ {
		if _, err = rand.Read(b); err == nil {
			return nil
		}
	}
	return err
}
