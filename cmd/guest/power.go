//go:build linux

package main

import (
	"errors"
	"slices"
)

// Power losses (GO's disk=power): a kind of kill that cuts the power of
// the nodes' disks too (see disk.go), as a power cut would. The nodes'
// containers, frozen, are killed with their disks shut down at once
// (writing nothing more, not even their journals); then each disk loses
// the writes its cache held since its last flush, some of each (see
// timeskip_cut in guest/kernel/timeskip.patch), and is mounted again,
// replaying its journal: what was committed stays, and what wasn't, data
// or metadata (a file created, renamed or removed), is gone, as after a
// real power cut. Volumes and project files that nodes share aren't on a
// node's disk, and are left alone.

// powerOff cuts the power of containers cs, frozen, all of their nodes'
// running containers.
func (in *injector) powerOff(cs []container) error {
	// (Not the disks of nodes excluded from disk faults: theirs is a kill.)
	ds := slices.DeleteFunc(nodeDisks(cs), func(d *disk) bool {
		return slices.ContainsFunc(in.nodes, func(n *node) bool {
			return n.excluded["disk"] && slices.ContainsFunc(slices.Concat(n.levels...), func(c container) bool { return disks[c.ID] == d })
		})
	})
	var errs []error
	for _, d := range ds {
		d.mu.Lock()
		errs = append(errs, d.shutdown())
	}
	// (Docker unfreezes a container it kills.)
	_, err := each(cs, killWait)
	errs = append(errs, err)
	for _, d := range ds {
		if !slices.ContainsFunc(errs, func(e error) bool { return e != nil }) {
			errs = append(errs, d.remount(true))
		}
		d.mu.Unlock()
	}
	return errors.Join(errs...)
}

// cache turns the volatile write caches of the node disks on (see
// timeskip_cache in guest/kernel/timeskip.patch), for power losses.
func cache() error {
	var errs []error
	done := map[*disk]bool{}
	for _, d := range disks {
		if !done[d] {
			done[d] = true
			errs = append(errs, d.sys0("cache", "1"))
		}
	}
	return errors.Join(errs...)
}
