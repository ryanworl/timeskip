//go:build linux

package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
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

// What the merge holds of a container's log is bounded: a line goes out
// in chunks of at most logLineMax bytes, all with the line's time; and when
// the container's lines held (in part, read ahead and the one to write)
// pass logsHeldMax, its oldest line in part goes out as it is, the rest of
// it following as further chunks.
var logLineMax, logsHeldMax = 1 << 20, 2 << 20

// A logStream is a container's log being merged.
type logStream struct {
	name, path string
	sc         *bufio.Scanner
	pos        int64               // the next entry's offset in the log
	parts      map[string]*logPart // each stream's line read in part
	ready      []logLine           // lines read, in order
	held       int                 // the bytes of parts, ready and line
	t          time.Duration       // the next line's guest uptime
	line       string              // ... and the line, "" at the end
}

// openLog opens the json-file log at path for the merge: what was written
// of it by now, and of that at most the last logsPerContainer bytes. skip
// is how much it leaves out at the start.
func openLog(name, path string) (s *logStream, f *os.File, skip int64, err error) {
	if f, err = os.Open(path); err != nil {
		return nil, nil, 0, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, 0, err
	}
	skip = max(0, st.Size()-logsPerContainer)
	s = &logStream{name: name, path: path, pos: skip, parts: map[string]*logPart{},
		sc: bufio.NewScanner(io.NewSectionReader(f, skip, st.Size()-skip))}
	s.sc.Buffer(make([]byte, 64<<10), 1<<20)
	if skip > 0 {
		s.sc.Scan() // (the rest of a line)
		s.pos += int64(len(s.sc.Bytes())) + 1
	}
	return s, f, skip, nil
}

// A logPart is a line Docker wrote in pieces (one per 16 KiB, and a line
// unended when its stream closed): each piece but the last has no newline,
// and all have the first's time.
type logPart struct {
	log  strings.Builder // (what's not gone out in chunks)
	time time.Time
	t    time.Duration // the line's guest uptime
	pos  int64         // the offset of log's first piece
	cut  bool          // whether some of it went out
}

type logLine struct {
	pos  int64
	t    time.Duration
	line string
}

// next reads s's next line, once the one before is written: in order of
// their first pieces, so a line read waits for the lines in part before it.
func (s *logStream) next(segs []clockSeg) {
	s.held -= len(s.line)
	for s.waiting() {
		if !s.sc.Scan() {
			// Lines unended at the end: their containers' last words.
			for stream, p := range s.parts {
				s.add(stream, p)
			}
			clear(s.parts)
			if len(s.ready) == 0 {
				s.line = ""
				return
			}
			break
		}
		var e struct {
			Log, Stream string
			Time        time.Time
		}
		pos := s.pos
		s.pos += int64(len(s.sc.Bytes())) + 1
		if json.Unmarshal(s.sc.Bytes(), &e) != nil {
			continue
		}
		p := s.parts[e.Stream]
		if p != nil && !p.time.Equal(e.Time) { // (a line unended when its stream closed, before a restart)
			s.add(e.Stream, p)
			p = nil
		}
		if p == nil {
			p = &logPart{time: e.Time, t: guestTime(e.Time, s.path, pos, segs), pos: pos}
		} else if p.log.Len() == 0 {
			p.pos = pos
		}
		p.log.WriteString(e.Log)
		s.held += len(e.Log)
		if strings.HasSuffix(e.Log, "\n") {
			s.add(e.Stream, p)
			delete(s.parts, e.Stream)
		} else {
			s.parts[e.Stream] = p
			if p.log.Len() >= logLineMax {
				s.add(e.Stream, p)
			}
		}
		for s.held > logsHeldMax && s.waiting() && len(s.ready) > 0 {
			stream := ""
			for k, p := range s.parts {
				if p.log.Len() > 0 && (stream == "" || p.pos < s.parts[stream].pos) {
					stream = k
				}
			}
			s.add(stream, s.parts[stream])
		}
	}
	s.t, s.line = s.ready[0].t, s.ready[0].line
	s.ready = s.ready[1:]
}

// waiting is whether s has no line read, or one after a line in part.
func (s *logStream) waiting() bool {
	if len(s.ready) == 0 {
		return true
	}
	for _, p := range s.parts {
		if p.log.Len() > 0 && p.pos < s.ready[0].pos {
			return true
		}
	}
	return false
}

// add adds what p of stream holds to s's lines read, in order: the rest of
// its line, or a chunk of it.
func (s *logStream) add(stream string, p *logPart) {
	text := strings.TrimSuffix(p.log.String(), "\n")
	if p.cut && text == "" { // (a newline after a chunk)
		s.held -= p.log.Len()
		p.log.Reset()
		return
	}
	l := logLine{p.pos, p.t, fmt.Sprintf("%+.3fs %s %s %s\n", (p.t - logsT0).Seconds(), s.name, stream, logText(text))}
	s.held += len(l.line) - p.log.Len()
	p.log.Reset()
	p.cut = true
	i, _ := slices.BinarySearchFunc(s.ready, l.pos, func(l logLine, pos int64) int { return cmp.Compare(l.pos, pos) })
	s.ready = slices.Insert(s.ready, i, l)
}

// logText is a log line's text as .containers.log has it, which a zero byte
// ends: a NUL shows as \x00, and the backslashes right before a NUL or an
// "x00" are doubled, so that the text \x00 shows as \\x00.
func logText(s string) string {
	if !strings.Contains(s, "\x00") && !strings.Contains(s, `\x00`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case 0:
			b.WriteString(`\x00`)
		case '\\':
			j := i
			for j < len(s) && s[j] == '\\' {
				j++
			}
			n := j - i
			if j < len(s) && (s[j] == 0 || strings.HasPrefix(s[j:], "x00")) {
				n *= 2
			}
			b.WriteString(strings.Repeat(`\`, n))
			i = j - 1
		default:
			b.WriteByte(s[i])
		}
	}
	return b.String()
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
		s, f, skip, err := openLog(name, in.LogPath)
		if err != nil {
			fmt.Fprintf(w, "# %s: no logs (logging driver %s)\n", name, in.HostConfig.LogConfig.Type)
			continue
		}
		defer f.Close()
		if skip > 0 {
			fmt.Fprintf(w, "# %s: the first %d MiB of its logs are left out\n", name, skip>>20)
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
