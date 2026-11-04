//go:build linux

// client contends for one lockd lock for a while, as a parallel_driver_
// command. It polls to acquire the lock, then works in steps of random
// length, renewing when less than half the TTL is left, and releases it
// after a few steps. It counts itself the holder until TTL after it sent its
// last successful acquire or renewal (not at all if the answer came later),
// and stops at the first step that ends later. In /tmp/holder (under flock)
// it notes its token and that deadline each time it gets or keeps the lock,
// by the monotonic clock, which every client shares and clock skips don't
// move; finding another client's unexpired deadline there means two clients
// hold the lock at once.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	lockd  = flag.String("lockd", "http://lockd:7000", "lockd's URL")
	name   = flag.String("lock", "the-lock", "the lock's name")
	run    = flag.Duration("for", 10*time.Minute, "how long to contend")
	step   = flag.Duration("step", 600*time.Millisecond, "the longest work step")
	poll   = flag.Duration("poll", 200*time.Millisecond, "the longest pause between attempts to acquire")
	client = http.Client{Timeout: 5 * time.Second}
	safe   = true
)

func main() {
	flag.Parse()
	for end := time.Now().Add(*run); time.Now().Before(end); time.Sleep(rand.N(*poll)) {
		sent := time.Now()
		body, ok := post("acquire", url.Values{"name": {*name}})
		if !ok {
			continue
		}
		var token uint64
		var ttlMS int64
		fmt.Sscan(body, &token, &ttlMS)
		ttl := time.Duration(ttlMS) * time.Millisecond
		until := sent.Add(ttl)
		if time.Until(until) <= 0 { // (the answer came after the lease ran out)
			continue
		}
		claim(token, until)
		held := true
		for n := 1 + rand.IntN(16); held && n > 0; n-- {
			time.Sleep(rand.N(*step)) // work
			left := time.Until(until)
			switch {
			case left <= 0:
				held = false
			case left < ttl/2:
				sent := time.Now()
				if _, held = post("renew", url.Values{"name": {*name}, "token": {fmt.Sprint(token)}}); held {
					until = sent.Add(ttl)
					if held = time.Until(until) > 0; held {
						claim(token, until)
					}
				}
			}
		}
		if held {
			claim(0, time.Time{})
			post("release", url.Values{"name": {*name}, "token": {fmt.Sprint(token)}})
		}
	}
	assert("always", "Always", "two clients never hold the lock at once", safe, nil)
}

func post(op string, v url.Values) (string, bool) {
	resp, err := client.PostForm(*lockd+"/"+op, v)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode == http.StatusOK
}

// claim notes in /tmp/holder that token holds the lock until until (none if
// 0), and asserts that no other client's deadline there is still ahead.
func claim(token uint64, until time.Time) {
	f, err := os.OpenFile("/tmp/holder", os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
	b, _ := io.ReadAll(f)
	fs := strings.Fields(string(b))
	if len(fs) == 2 {
		other, _ := strconv.ParseUint(fs[0], 10, 64)
		otherUntil, _ := strconv.ParseInt(fs[1], 10, 64)
		left := time.Duration(otherUntil-monotonicMs()) * time.Millisecond
		if token != 0 && other != 0 && other != token {
			assert("sometimes", "Sometimes", "a client takes over the lock from a holder whose lease ran out", left <= 0, nil)
			if left > 0 {
				safe = false
				assert("always", "Always", "two clients never hold the lock at once", false,
					map[string]any{"holder": other, "holder_ms_left": left.Milliseconds(), "new_holder": token})
			}
		}
	}
	var ms int64
	if token != 0 {
		ms = monotonicMs() + time.Until(until).Milliseconds()
	}
	f.Truncate(0)
	f.WriteAt([]byte(fmt.Sprintf("%d %d\n", token, ms)), 0)
}

// monotonicMs is the machine's CLOCK_MONOTONIC in ms. (time.Now's
// monotonic reading is the process's own.)
func monotonicMs() int64 {
	var ts syscall.Timespec
	syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0) // CLOCK_MONOTONIC
	return ts.Nano() / 1e6
}

// assert appends an Antithesis SDK assertion to $ANTITHESIS_OUTPUT_DIR/sdk.jsonl.
func assert(kind, display, message string, cond bool, details map[string]any) {
	b, _ := json.Marshal(map[string]any{"antithesis_assert": map[string]any{"assert_type": kind, "display_type": display,
		"message": message, "id": message, "condition": cond, "hit": true, "must_hit": true, "details": details}})
	f, err := os.OpenFile(os.Getenv("ANTITHESIS_OUTPUT_DIR")+"/sdk.jsonl", os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
	if err != nil {
		panic(err)
	}
	f.Write(append(b, '\n'))
	f.Close()
}
