// virtio-mmio (version 2) devices: console, block and entropy. Each runs on
// the vCPU thread, when the guest writes to its registers, so no device
// state is shared between threads except the console's pending input.
import Foundation
import Hypervisor

// Guest RAM, mapped at base in the guest's physical address space.
struct Mem {
	let base: UInt64, size: UInt64
	let host: UnsafeMutableRawPointer

	@inline(__always) func ptr(_ a: UInt64, _ n: Int) -> UnsafeMutableRawPointer {
		guard a >= base, UInt64(n) <= size, a - base <= size - UInt64(n) else {
			fail(String(format: "guest address 0x%llx (+%d) is outside RAM", a, n))
		}
		return host + Int(a - base)
	}
	func u16(_ a: UInt64) -> UInt16 { ptr(a, 2).loadUnaligned(as: UInt16.self) }
	func u32(_ a: UInt64) -> UInt32 { ptr(a, 4).loadUnaligned(as: UInt32.self) }
	func u64(_ a: UInt64) -> UInt64 { ptr(a, 8).loadUnaligned(as: UInt64.self) }
	func put16(_ a: UInt64, _ v: UInt16) { ptr(a, 2).storeBytes(of: v, as: UInt16.self) }
	func put32(_ a: UInt64, _ v: UInt32) { ptr(a, 4).storeBytes(of: v, as: UInt32.self) }
}

var mem: Mem!

let VIRTIO_F_VERSION_1: UInt64 = 1 << 32
let queueMax: UInt32 = 256

struct Queue: Codable {
	var size: UInt32 = 0, ready: UInt32 = 0
	var desc: UInt64 = 0, avail: UInt64 = 0, used: UInt64 = 0
	var next: UInt16 = 0 // the next available ring entry to take
}

struct Desc {
	let addr: UInt64, len: Int, write: Bool
}

// What a device's transport saves: everything the guest set up.
struct DeviceState: Codable {
	var status: UInt32, featSel: UInt32, drvFeatSel: UInt32, queueSel: UInt32
	var intStatus: UInt32, configGen: UInt32, line: Bool
	var driverFeatures: UInt64
	var queues: [Queue]
	var capacity: UInt64 // (virtio-blk) the disk's size when saved
}

protocol VirtioDevice: AnyObject {
	var id: UInt32 { get }
	var features: UInt64 { get }
	var queueCount: Int { get }
	var capacity: UInt64 { get }
	func config(_ off: Int) -> UInt8
	func notify(_ q: Int, _ t: Transport)
}

extension VirtioDevice {
	var capacity: UInt64 { 0 }
	func config(_ off: Int) -> UInt8 { 0 }
}

final class Transport {
	let dev: VirtioDevice
	let spi: UInt32
	var status: UInt32 = 0, featSel: UInt32 = 0, drvFeatSel: UInt32 = 0, queueSel: UInt32 = 0
	var intStatus: UInt32 = 0, configGen: UInt32 = 0, line = false
	var driverFeatures: UInt64 = 0
	var queues: [Queue]

	init(_ dev: VirtioDevice, spi: UInt32) {
		self.dev = dev
		self.spi = spi
		queues = Array(repeating: Queue(), count: dev.queueCount)
	}

	func read(_ off: UInt64, _ len: Int) -> UInt64 {
		if off >= 0x100 {
			var v: UInt64 = 0
			for i in 0..<len { v |= UInt64(dev.config(Int(off) - 0x100 + i)) << (8 * i) }
			return v
		}
		let q = Int(queueSel) < queues.count ? queues[Int(queueSel)] : nil
		switch off {
		case 0x000: return 0x7472_6976 // "virt"
		case 0x004: return 2
		case 0x008: return UInt64(dev.id)
		case 0x00c: return 0x5453_4b50 // vendor
		case 0x010: return featSel < 2 ? (dev.features >> (32 * UInt64(featSel))) & 0xffff_ffff : 0
		case 0x034: return q == nil ? 0 : UInt64(queueMax)
		case 0x044: return UInt64(q?.ready ?? 0)
		case 0x060: return UInt64(intStatus)
		case 0x070: return UInt64(status)
		case 0x0b0, 0x0b4: return 0xffff_ffff // no shared memory regions
		case 0x0fc: return UInt64(configGen)
		default: return 0
		}
	}

	func write(_ off: UInt64, _ v64: UInt64) {
		let v = UInt32(truncatingIfNeeded: v64)
		let qi = Int(queueSel)
		func setQ(_ f: (inout Queue) -> Void) { if qi < queues.count { f(&queues[qi]) } }
		func lo(_ x: inout UInt64) { x = (x & ~0xffff_ffff) | UInt64(v) }
		func hi(_ x: inout UInt64) { x = (x & 0xffff_ffff) | UInt64(v) << 32 }
		switch off {
		case 0x014: featSel = v
		case 0x020:
			if drvFeatSel < 2 {
				let s = 32 * UInt64(drvFeatSel)
				driverFeatures = (driverFeatures & ~(0xffff_ffff << s)) | UInt64(v) << s
			}
		case 0x024: drvFeatSel = v
		case 0x030: queueSel = v
		case 0x038: setQ { $0.size = v }
		case 0x044:
			setQ {
				if v != 0 && ($0.size == 0 || $0.size > queueMax || $0.size & ($0.size - 1) != 0) { fail("virtio queue size \($0.size)") }
				$0.ready = v
			}
		case 0x050: if Int(v) < queues.count && queues[Int(v)].ready != 0 { dev.notify(Int(v), self) }
		case 0x064:
			intStatus &= ~v
			updateLine()
		case 0x070:
			status = v
			if v == 0 { // reset
				queues = Array(repeating: Queue(), count: queues.count)
				intStatus = 0
				driverFeatures = 0
				updateLine()
			}
		case 0x080: setQ { lo(&$0.desc) }
		case 0x084: setQ { hi(&$0.desc) }
		case 0x090: setQ { lo(&$0.avail) }
		case 0x094: setQ { hi(&$0.avail) }
		case 0x0a0: setQ { lo(&$0.used) }
		case 0x0a4: setQ { hi(&$0.used) }
		default: break
		}
	}

	func updateLine() {
		let l = intStatus != 0
		if l != line {
			line = l
			check(hv_gic_set_spi(spi, l), "hv_gic_set_spi")
		}
	}

	// interrupt raises a used-buffer (1) or configuration-change (2) interrupt.
	func interrupt(_ bit: UInt32 = 1) {
		intStatus |= bit
		updateLine()
	}

	func hasInput(_ qi: Int) -> Bool {
		let q = queues[qi]
		return q.ready != 0 && mem.u16(q.avail + 2) != q.next
	}

	// pop takes the next descriptor chain the driver made available.
	func pop(_ qi: Int) -> (UInt16, [Desc])? {
		guard hasInput(qi) else { return nil }
		let q = queues[qi]
		let head = mem.u16(q.avail + 4 + 2 * UInt64(Int(q.next) & Int(q.size - 1)))
		queues[qi].next &+= 1
		var chain: [Desc] = []
		var i = head
		while true {
			let d = q.desc + 16 * UInt64(i)
			let flags = mem.u16(d + 12)
			if flags & 4 != 0 { fail("indirect descriptor (not offered)") }
			chain.append(Desc(addr: mem.u64(d), len: Int(mem.u32(d + 8)), write: flags & 2 != 0))
			if flags & 1 == 0 { break }
			i = mem.u16(d + 14)
			if chain.count > Int(q.size) { fail("descriptor loop") }
		}
		return (head, chain)
	}

	// push returns a chain to the driver, with len bytes written to it.
	func push(_ qi: Int, _ head: UInt16, _ len: Int) {
		let q = queues[qi]
		let idx = mem.u16(q.used + 2)
		let e = q.used + 4 + 8 * UInt64(Int(idx) & Int(q.size - 1))
		mem.put32(e, UInt32(head))
		mem.put32(e + 4, UInt32(len))
		mem.put16(q.used + 2, idx &+ 1)
	}

	var state: DeviceState {
		DeviceState(status: status, featSel: featSel, drvFeatSel: drvFeatSel, queueSel: queueSel,
			intStatus: intStatus, configGen: configGen, line: line, driverFeatures: driverFeatures,
			queues: queues, capacity: dev.capacity)
	}

	func restore(_ s: DeviceState) {
		status = s.status; featSel = s.featSel; drvFeatSel = s.drvFeatSel; queueSel = s.queueSel
		intStatus = s.intStatus; configGen = s.configGen; line = s.line
		driverFeatures = s.driverFeatures; queues = s.queues
		// (The GIC's saved state needn't hold the line's level.)
		check(hv_gic_set_spi(spi, line), "hv_gic_set_spi")
		// A disk that changed size tells the driver so.
		if s.capacity != dev.capacity {
			configGen &+= 1
			interrupt(2)
		}
	}
}

func writeAll(_ fd: Int32, _ p: UnsafeRawPointer, _ n: Int) {
	var done = 0
	while done < n {
		let r = Darwin.write(fd, p + done, n - done)
		if r < 0 {
			if errno == EINTR || errno == EAGAIN { continue }
			return
		}
		done += r
	}
}

// The console: hvc0. Output goes to stdout; input arrives from the stdin
// thread into pending.
final class Console: VirtioDevice {
	let id: UInt32 = 3
	let features = VIRTIO_F_VERSION_1
	let queueCount = 2
	let lock = NSLock()
	var pending: [UInt8] = []

	func notify(_ q: Int, _ t: Transport) {
		if q == 0 {
			deliver(t)
			return
		}
		var any = false
		while let (head, chain) = t.pop(1) {
			for d in chain where !d.write { writeAll(1, mem.ptr(d.addr, d.len), d.len) }
			t.push(1, head, 0)
			any = true
		}
		if any { t.interrupt() }
	}

	func input(_ b: [UInt8]) {
		lock.lock()
		pending += b
		lock.unlock()
	}

	// deliver copies pending input into the receive queue's buffers.
	func deliver(_ t: Transport) {
		lock.lock()
		defer { lock.unlock() }
		var any = false
		while !pending.isEmpty, let (head, chain) = t.pop(0) {
			var n = 0
			for d in chain where d.write && !pending.isEmpty {
				let k = min(d.len, pending.count)
				pending.withUnsafeBytes { mem.ptr(d.addr, k).copyMemory(from: $0.baseAddress!, byteCount: k) }
				pending.removeFirst(k)
				n += k
			}
			t.push(0, head, n)
			any = true
		}
		if any { t.interrupt() }
	}
}

// hostIO stops hvrun if a disk read or write (its result r) didn't
// transfer all n bytes.
func hostIO(_ r: Int, _ n: Int, _ what: String) {
	if r != n { fail("disk \(what): " + (r < 0 ? String(cString: strerror(errno)) : "\(r) of \(n) bytes")) }
}

// A disk: a raw image file.
final class Blk: VirtioDevice {
	let id: UInt32 = 2
	let features: UInt64
	let queueCount = 1
	let fd: Int32, ro: Bool
	let capacity: UInt64 // in 512-byte sectors

	init(_ path: String, ro: Bool) {
		fd = open(path, ro ? O_RDONLY : O_RDWR)
		if fd < 0 { fail("open \(path): \(String(cString: strerror(errno)))") }
		var st = stat()
		fstat(fd, &st)
		capacity = UInt64(st.st_size) / 512
		self.ro = ro
		features = VIRTIO_F_VERSION_1 | 1 << 2 | (ro ? 1 << 5 : 0) // SEG_MAX, RO
	}

	// capacity, and seg_max: requests of up to 126 segments (without it,
	// Linux makes every request one segment).
	func config(_ off: Int) -> UInt8 {
		switch off {
		case 0..<8: return UInt8(truncatingIfNeeded: capacity >> (8 * UInt64(off)))
		case 12: return 126
		default: return 0
		}
	}

	func notify(_ q: Int, _ t: Transport) {
		var any = false
		while let (head, chain) = t.pop(0) {
			any = true
			guard chain.count >= 2, let st = chain.last, st.write else { fail("bad virtio-blk request") }
			let type = mem.u32(chain[0].addr)
			let sector = mem.u64(chain[0].addr + 8)
			var off = off_t(min(sector, capacity) * 512)
			var status: UInt8 = type == 0 || type == 1 ? 0 : 2, written = 0 // IN and OUT; else UNSUPP
			for d in chain[1..<chain.count - 1] where status != 2 {
				let p = mem.ptr(d.addr, d.len)
				// (Reads and writes past the end fail rather than grow the image.)
				if UInt64(off) + UInt64(d.len) > capacity * 512 { status = 1; break }
				// (The host failing a read or write, as a full disk does, is
				// no fault of the guest's: hvrun stops, rather than the guest
				// seeing an I/O error the test would blame on the project.)
				if type == 0 {
					hostIO(pread(fd, p, d.len, off), d.len, "read")
					written += d.len
				} else if ro {
					status = 1
				} else {
					hostIO(pwrite(fd, p, d.len, off), d.len, "write")
				}
				off += off_t(d.len)
			}
			mem.ptr(st.addr, 1).storeBytes(of: status, as: UInt8.self)
			t.push(0, head, written + 1)
		}
		if any { t.interrupt() }
	}
}

// Entropy, from the host's generator.
final class Rng: VirtioDevice {
	let id: UInt32 = 4
	let features = VIRTIO_F_VERSION_1
	let queueCount = 1

	func notify(_ q: Int, _ t: Transport) {
		var any = false
		while let (head, chain) = t.pop(0) {
			var n = 0
			for d in chain where d.write {
				arc4random_buf(mem.ptr(d.addr, d.len), d.len)
				n += d.len
			}
			t.push(0, head, n)
			any = true
		}
		if any { t.interrupt() }
	}
}
