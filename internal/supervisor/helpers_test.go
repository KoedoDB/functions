package supervisor

import (
	"net/http"
	"testing"
	"time"
)

// What the tests of the supervisor share.

type answer struct {
	status int
	body   string
	header http.Header
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("not true in time: %s", what)
}
