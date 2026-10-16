package opencode

import (
	"crypto/rand"
	"math/big"
	"regexp"
	"strings"
	"sync"
	"time"
)

// How this server speaks as OpenCode: the version it introduces itself with,
// and the ids it makes in OpenCode's own shape (provider.mjs).

// The OpenCode version whose requests are imitated, and the ai-sdk
// provider-utils version each protocol's package bundles, as captured from
// OpenCode's own requests.
const Version = "1.19.0"

var providerUtils = map[string]string{
	"chat":      "4.0.23",
	"responses": "4.0.40",
	"anthropic": "4.0.46",
}

func UserAgent(protocol string) string {
	return "opencode/" + Version + " ai-sdk/provider-utils/" + providerUtils[protocol] +
		" runtime/bun/1.3.14"
}

const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

var idMu sync.Mutex

type idState struct {
	lastStamp int64
	counter   int64
}

var ids = idState{}

// ID builds an identifier in OpenCode's own shape.
func ID(prefix string, descending bool, now time.Time) string {
	idMu.Lock()
	stamp := now.UnixMilli()
	if stamp != ids.lastStamp {
		ids.lastStamp = stamp
		ids.counter = 0
	}
	ids.counter++
	counter := ids.counter
	idMu.Unlock()

	current := new(big.Int).SetUint64(uint64(stamp))
	current.Mul(current, big.NewInt(0x1000))
	current.Add(current, big.NewInt(counter))
	if descending {
		current.Not(current)
	}
	mask := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 48), big.NewInt(1))
	current.And(current, mask)

	head := current.Text(16)
	if len(head) < 12 {
		head = strings.Repeat("0", 12-len(head)) + head
	}

	raw := make([]byte, 14)
	if _, err := rand.Read(raw); err != nil {
		for i := range raw {
			raw[i] = byte(i * 7)
		}
	}
	var tail strings.Builder
	for _, value := range raw {
		tail.WriteByte(alphabet[int(value)%len(alphabet)])
	}
	return prefix + "_" + head + tail.String()
}

var realSessionPattern = regexp.MustCompile(`^ses_[0-9a-f]{9}([0-9a-f]{3})[0-9A-Za-z]{14}$`)

// RealSessionID reports whether a session id is one OpenCode itself could have
// made: its counter (the low 12 bits, inverted) is small.
func RealSessionID(value string) bool {
	match := realSessionPattern.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	var counter int
	for _, digit := range match[1] {
		counter <<= 4
		n := strings.IndexRune("0123456789abcdef", digit)
		if n < 0 {
			return false
		}
		counter |= n
	}
	return 0xfff-counter < 16
}
