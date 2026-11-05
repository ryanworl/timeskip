//go:build linux

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ryanworl/timeskip/internal/protocol"
)

// Container logs: a failed timeline's (or setup's) go to the host whole,
// every container's stdout and stderr as Docker kept them (json-file, on
// Docker's disk), merged in order of guest time. The agent writes them once,
// at the end, raw to the data disk past Docker's filesystem
// (protocol.ContainerLogsAt), and the CLI reads them from its copy of the
// disk: nothing leaves the VM while the timeline runs, where the I/O would
// keep idle time from skipping and change what its replays do.

var (
	logsT0     time.Duration // the guest uptime the logs' times count from: setup's or the timeline's start
	clockMu    sync.Mutex
	clockSteps []clockStep // CLOCK_REALTIME's steps (see skip), in order
)

// A clockStep stepped CLOCK_REALTIME by d at uptime at, when the
// containers' logs (json-file) were sizes long: which of their lines were
// timed before it.
type clockStep struct {
	at, d time.Duration
	sizes map[string]int64
}

// logSizes are the sizes of the containers' logs, by path.
func logSizes() map[string]int64 {
	m := map[string]int64{}
	paths, _ := filepath.Glob("/var/lib/docker/containers/*/*-json.log")
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil {
			m[p] = fi.Size()
		}
	}
	return m
}

// logsPerContainer is how much of a container's json-file log goes to the
// host: the end of it.
const logsPerContainer = 32 << 20

// A logStream is a container's log being merged.
type logStream struct {
	name, path string
	sc         *bufio.Scanner
	pos        int64         // the next line's offset in the log
	t          time.Duration // ... its guest uptime
	line       string        // ... and the line, "" at the end
}

// next reads s's next line.
func (s *logStream) next(segs []clockSeg) {
	for s.line = ""; s.sc.Scan(); {
		var e struct {
			Log, Stream string
			Time        time.Time
		}
		pos := s.pos
		s.pos += int64(len(s.sc.Bytes())) + 1
		if json.Unmarshal(s.sc.Bytes(), &e) != nil {
			continue
		}
		s.t = guestTime(e.Time, s.path, pos, segs)
		s.line = fmt.Sprintf("%+.3fs %s %s %s\n", (s.t - logsT0).Seconds(), s.name, e.Stream,
			strings.ReplaceAll(strings.TrimSuffix(e.Log, "\n"), "\x00", `\x00`))
		return
	}
}

// A clockSeg is a stretch of uptime, from from, when CLOCK_REALTIME read
// uptime+off, and the containers' logs were sizes long at its start.
type clockSeg struct {
	from, off time.Duration
	sizes     map[string]int64
}

// guestTime is the guest uptime of the line at pos in the log at path, which
// Docker timed wall: the line is in the stretch of the clock that its log
// was written up to it in; or in the one before, if Docker timed it before
// the step and wrote it after the agent took the log's size.
func guestTime(wall time.Time, path string, pos int64, segs []clockSeg) time.Duration {
	k := 0
	for i := 1; i < len(segs); i++ {
		if segs[i].sizes[path] <= pos {
			k = i
		}
	}
	w := time.Duration(wall.UnixNano())
	b := w - segs[k].off
	if k > 0 && b < segs[k].from { // (then timed before the step)
		if before := w - segs[k-1].off; before < segs[k].from {
			return before
		}
	}
	return b
}

// dumpLogs writes every container's logs to the data disk, as lines of
// guest time since logsT0, the container's name (its service and number if
// it's the project's), its stream and the line, ended by a zero byte.
func dumpLogs() error {
	clockMu.Lock()
	steps := clockSteps
	clockMu.Unlock()
	now := uptime()
	off := time.Duration(time.Now().UnixNano()) - now
	for _, s := range steps {
		off -= s.d
	}
	segs := []clockSeg{{0, off, nil}}
	for _, s := range steps {
		off += s.d
		segs = append(segs, clockSeg{s.at, off, s.sizes})
	}
	var list []struct {
		ID     string
		Names  []string
		Labels map[string]string
	}
	if err := call("GET", "/containers/json?all=1", nil, &list); err != nil {
		return err
	}
	disk, err := os.OpenFile("/dev/vdb", os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer disk.Close()
	w := bufio.NewWriterSize(io.NewOffsetWriter(disk, protocol.ContainerLogsAt), 1<<20)
	var streams []*logStream
	for _, c := range list {
		name := strings.TrimPrefix(c.Names[0], "/")
		if svc := c.Labels["com.docker.compose.service"]; svc != "" && c.Labels["com.docker.compose.project"] == project {
			name = svc + "-" + c.Labels["com.docker.compose.container-number"]
		}
		var in struct {
			LogPath    string
			HostConfig struct{ LogConfig struct{ Type string } }
		}
		if call("GET", "/containers/"+c.ID+"/json", nil, &in) != nil {
			continue
		}
		f, err := os.Open(in.LogPath)
		if err != nil {
			fmt.Fprintf(w, "# %s: no logs (logging driver %s)\n", name, in.HostConfig.LogConfig.Type)
			continue
		}
		defer f.Close()
		s := &logStream{name: name, path: in.LogPath, sc: bufio.NewScanner(f)}
		s.sc.Buffer(make([]byte, 64<<10), 1<<20)
		if st, _ := f.Stat(); st.Size() > logsPerContainer {
			s.pos, _ = f.Seek(st.Size()-logsPerContainer, io.SeekStart)
			s.sc.Scan() // (the rest of a line)
			s.pos += int64(len(s.sc.Bytes())) + 1
			fmt.Fprintf(w, "# %s: the first %d MiB of its logs are left out\n", name, (st.Size()-logsPerContainer)>>20)
		}
		if s.next(segs); s.line != "" {
			streams = append(streams, s)
		}
	}
	n := w.Buffered()
	for len(streams) > 0 {
		i := 0
		for j, s := range streams {
			if s.t < streams[i].t {
				i = j
			}
		}
		s := streams[i]
		if n += len(s.line); n >= protocol.ContainerLogsSize-64 {
			fmt.Fprintf(w, "# the rest is left out (over %d MiB)\n", protocol.ContainerLogsSize>>20)
			break
		}
		w.WriteString(s.line)
		if s.next(segs); s.line == "" {
			streams = append(streams[:i], streams[i+1:]...)
		}
	}
	w.WriteByte(0)
	if err := w.Flush(); err != nil {
		return err
	}
	return disk.Sync()
}
