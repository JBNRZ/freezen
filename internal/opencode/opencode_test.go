package opencode

import (
	"strings"
	"testing"
	"time"
)

func testNow() time.Time { return time.UnixMilli(1700000000000) }

func TestIDShape(t *testing.T) {
	id := ID("ses", true, testNow())
	if !strings.HasPrefix(id, "ses_") {
		t.Fatalf("session id %q has no prefix", id)
	}
	if !RealSessionID(id) {
		t.Fatalf("session id %q is not one OpenCode could have made", id)
	}
	if got := len(id); got != 4+12+14 {
		t.Fatalf("session id %q has length %d", id, got)
	}
}

func TestRealSessionIDRejectsACounterThatIsTooLarge(t *testing.T) {
	if RealSessionID("ses_" + strings.Repeat("0", 26)) {
		t.Fatal("a session id with a large counter was taken for a real one")
	}
}

func TestUserAgentNamesTheOpenCodeVersion(t *testing.T) {
	if got := UserAgent("chat"); !strings.HasPrefix(got, "opencode/"+Version) {
		t.Fatalf("the user-agent is %q", got)
	}
}
