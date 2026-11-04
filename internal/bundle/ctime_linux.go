package bundle

import (
	"syscall"
	"time"
)

func ctimeOf(st *syscall.Stat_t) time.Time { return time.Unix(st.Ctim.Unix()) }
