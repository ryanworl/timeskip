//go:build linux

package main

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// slowWriter takes its first write slowly, as a slow console does.
type slowWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	slow bool
}

func (w *slowWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.slow {
		w.slow = true
		time.Sleep(1500 * time.Millisecond)
	}
	return w.buf.Write(p)
}

// A command's output written before it exited is copied whole, though the
// writer takes longer than the second after the exit; and what it left
// running, writing on, isn't waited for much past that second.
func TestCopyOutput(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	defer w.Close() // (what it left running holds the pipe open)
	out, exited, copied := &slowWriter{}, make(chan int, 1), make(chan struct{})
	go func() {
		copyOutput(r, out, exited)
		close(copied)
	}()
	before := strings.Repeat("a", 40000) + "\nEND\n"
	w.WriteString(before)
	time.Sleep(100 * time.Millisecond) // (the copier is writing its first chunk)
	exited <- queued(r)
	r.SetReadDeadline(time.Now())
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(10 * time.Millisecond):
				w.WriteString("left running\n")
			}
		}
	}()
	select {
	case <-copied:
	case <-time.After(10 * time.Second):
		t.Fatal("still copying 10 s after the exit")
	}
	out.mu.Lock()
	defer out.mu.Unlock()
	if !strings.HasPrefix(out.buf.String(), before) {
		t.Errorf("copied %d bytes, the first %d not all the %d written before the exit", out.buf.Len(), min(out.buf.Len(), len(before)), len(before))
	}
}
