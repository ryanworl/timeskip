// lockd is a lock service with leases. POST /acquire?name= grants a free
// lock and answers "TOKEN TTL_MS"; the lock is the holder's until its lease
// runs out, TTL after the grant or the last renewal (POST /renew?name=&token=),
// or until POST /release?name=&token=. Each grant and renewal is replicated to
// a standby (lockd -standby=) before it's acknowledged. A restarted lockd has
// forgotten its leases, so it grants nothing for a TTL.
//
// With LOCKD_FIX=1, a renewal checks again, after replicating, that its lease
// didn't run out meanwhile.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"
)

type lease struct {
	token    uint64
	deadline time.Time
	timer    *time.Timer
}

type server struct {
	ttl     time.Duration
	standby string
	fix     bool
	started time.Time
	client  http.Client

	mu     sync.Mutex
	tokens uint64
	leases map[string]*lease
}

func main() {
	addr := flag.String("addr", ":7000", "address to listen on")
	standby := flag.String("standby", "", "the standby's URL (none: be the standby)")
	ttl := flag.Duration("ttl", 2*time.Second, "lease TTL")
	flag.Parse()
	if *standby == "" {
		log.Fatal(http.ListenAndServe(*addr, standbyHandler()))
	}
	s := &server{ttl: *ttl, standby: *standby, fix: os.Getenv("LOCKD_FIX") == "1", started: time.Now(),
		client: http.Client{Timeout: 2 * time.Second}, tokens: uint64(time.Now().UnixNano()), leases: map[string]*lease{}}
	http.HandleFunc("GET /health", func(http.ResponseWriter, *http.Request) {})
	http.HandleFunc("POST /acquire", s.acquire)
	http.HandleFunc("POST /renew", s.renew)
	http.HandleFunc("POST /release", s.release)
	log.Printf("lockd: ttl %v, fix %v", s.ttl, s.fix)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func (s *server) acquire(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	s.mu.Lock()
	if s.leases[name] != nil || time.Since(s.started) < s.ttl {
		s.mu.Unlock()
		http.Error(w, "held", http.StatusConflict)
		return
	}
	s.tokens++
	l := &lease{token: s.tokens, deadline: time.Now().Add(s.ttl)}
	l.timer = time.AfterFunc(s.ttl, func() { s.expire(name, l) })
	s.leases[name] = l
	s.mu.Unlock()
	if err := s.journal("grant", name, l.token, l.deadline); err != nil {
		s.drop(name, l)
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	fmt.Fprintf(w, "%d %d\n", l.token, s.ttl.Milliseconds())
}

func (s *server) renew(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	token, _ := strconv.ParseUint(r.FormValue("token"), 10, 64)
	s.mu.Lock()
	l := s.leases[name]
	s.mu.Unlock()
	if l == nil || l.token != token {
		http.Error(w, "not held", http.StatusConflict)
		return
	}
	// Replicate first, without holding the mutex across the network call.
	deadline := time.Now().Add(s.ttl)
	if err := s.journal("renew", name, token, deadline); err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fix && s.leases[name] != l {
		http.Error(w, "expired while renewing", http.StatusConflict)
		return
	}
	l.deadline = deadline
	l.timer.Reset(time.Until(deadline))
	fmt.Fprintln(w, "ok")
}

func (s *server) release(w http.ResponseWriter, r *http.Request) {
	name := r.FormValue("name")
	token, _ := strconv.ParseUint(r.FormValue("token"), 10, 64)
	s.mu.Lock()
	l := s.leases[name]
	s.mu.Unlock()
	if l == nil || l.token != token {
		http.Error(w, "not held", http.StatusConflict)
		return
	}
	s.drop(name, l)
}

// expire runs when a lease's timer fires. (A renewal may have moved the
// deadline after the timer fired but before expire got the mutex.)
func (s *server) expire(name string, l *lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[name] == l && !time.Now().Before(l.deadline) {
		delete(s.leases, name)
	}
}

func (s *server) drop(name string, l *lease) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.leases[name] == l {
		l.timer.Stop()
		delete(s.leases, name)
	}
}

func (s *server) journal(op, name string, token uint64, deadline time.Time) error {
	resp, err := s.client.PostForm(s.standby+"/journal", url.Values{"op": {op}, "name": {name},
		"token": {strconv.FormatUint(token, 10)}, "deadline": {strconv.FormatInt(deadline.UnixMilli(), 10)}})
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("standby: %s", resp.Status)
	}
	return nil
}

// standbyHandler keeps the latest grant or renewal of each lock, to take
// over from (failing over isn't part of this example).
func standbyHandler() http.Handler {
	var mu sync.Mutex
	latest := map[string]string{}
	h := http.NewServeMux()
	h.HandleFunc("POST /journal", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		latest[r.FormValue("name")] = r.FormValue("op") + " " + r.FormValue("token") + " " + r.FormValue("deadline")
		mu.Unlock()
	})
	return h
}
