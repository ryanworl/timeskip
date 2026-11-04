// A read-only virtio-fs device: a FUSE server for one host directory, with
// just the requests that reading files and listing directories need.
import Foundation

// Linux's numbers for the errors that macOS numbers differently.
let linuxErrno: [Int32: Int32] = [ENOSYS: 38, ELOOP: 40, ENAMETOOLONG: 36, ENOTEMPTY: 39, ENOATTR: 61, ENOTSUP: 95]

final class FS: VirtioDevice {
	let id: UInt32 = 26
	let features = VIRTIO_F_VERSION_1
	let queueCount = 2 // hiprio, one request queue
	let tag: [UInt8], root: String
	var paths: [UInt64: String] = [1: ""], nodes: [String: UInt64] = ["": 1]
	var dirs: [UInt64: [(name: String, ino: UInt64, type: UInt32)]] = [:]
	var files: [UInt64: Int32] = [:]
	var nextNode: UInt64 = 2, nextDir: UInt64 = 1, nextFile: UInt64 = 1
	// Mounted between INIT and DESTROY. Its tables and open files aren't
	// saved, so a save must wait until it's unmounted.
	var mounted = false

	init(tag: String, root: String) {
		self.tag = Array(tag.utf8)
		self.root = root
	}

	func config(_ off: Int) -> UInt8 {
		if off < 36 { return off < tag.count ? tag[off] : 0 }
		return off == 36 ? 1 : 0 // num_request_queues
	}

	func notify(_ q: Int, _ t: Transport) {
		var any = false
		while let (head, chain) = t.pop(q) {
			any = true
			var req: [UInt8] = []
			for d in chain where !d.write { req += UnsafeRawBufferPointer(start: mem.ptr(d.addr, d.len), count: d.len) }
			var n = 0
			if let reply = handle(req) {
				for d in chain where d.write && n < reply.count {
					let k = min(d.len, reply.count - n)
					reply.withUnsafeBytes { mem.ptr(d.addr, k).copyMemory(from: $0.baseAddress! + n, byteCount: k) }
					n += k
				}
			}
			t.push(q, head, n)
		}
		if any { t.interrupt() }
	}

	func full(_ node: UInt64) -> String? { paths[node].map { root + "/" + $0 } }

	func handle(_ req: [UInt8]) -> [UInt8]? {
		// (A field past the request's end reads as 0.)
		func u32(_ o: Int) -> UInt32 { o + 4 > req.count ? 0 : req.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: o, as: UInt32.self) } }
		func u64(_ o: Int) -> UInt64 { o + 8 > req.count ? 0 : req.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: o, as: UInt64.self) } }
		guard req.count >= 40 else { return nil }
		let opcode = u32(4), unique = u64(8), node = u64(16)
		var out = Out(), err: Int32 = 0
		func fail(_ e: Int32) { err = -(linuxErrno[e] ?? e) }
		switch opcode {
		case 26: // INIT
			for fd in files.values { close(fd) }
			paths = [1: ""]; nodes = ["": 1]; dirs = [:]; files = [:]
			mounted = true
			out.u32(7); out.u32(31); out.u32(u32(48)); out.u32(1 << 22) // major, minor, max_readahead, flags: MAX_PAGES
			out.u16(16); out.u16(12); out.u32(1 << 20); out.u32(1); out.u16(256) // max_background, congestion, max_write, time_gran, max_pages
			out.zeros(2 + 4 + 4 + 24)
		case 1: // LOOKUP
			let name = String(decoding: req[40...].prefix { $0 != 0 }, as: UTF8.self)
			if name.isEmpty || name == "." || name == ".." || name.contains("/") { fail(EINVAL); break }
			guard let parent = paths[node] else { fail(ENOENT); break }
			let path = parent.isEmpty ? name : parent + "/" + name
			var st = stat()
			if lstat(root + "/" + path, &st) != 0 { fail(errno); break }
			let n = nodes[path] ?? nextNode
			if n == nextNode { nextNode += 1; nodes[path] = n; paths[n] = path }
			out.u64(n); out.u64(0); out.u64(86400); out.u64(86400); out.u32(0); out.u32(0)
			out.attr(st)
		case 2, 42, 36: return nil // FORGET, BATCH_FORGET, INTERRUPT: no reply
		case 3: // GETATTR
			var st = stat()
			guard let p = full(node) else { fail(ENOENT); break }
			if lstat(p, &st) != 0 { fail(errno); break }
			out.u64(86400); out.u32(0); out.u32(0)
			out.attr(st)
		case 5: // READLINK
			var buf = [CChar](repeating: 0, count: Int(PATH_MAX))
			guard let p = full(node) else { fail(ENOENT); break }
			let n = readlink(p, &buf, buf.count)
			if n < 0 { fail(errno); break }
			out.b += buf[0..<n].map { UInt8(bitPattern: $0) }
		case 14: // OPEN
			if u32(40) & 3 != 0 { fail(EROFS); break }
			guard let p = full(node) else { fail(ENOENT); break }
			let fd = open(p, O_RDONLY)
			if fd < 0 { fail(errno); break }
			files[nextFile] = fd
			out.u64(nextFile); out.u32(0); out.u32(0)
			nextFile += 1
		case 27: // OPENDIR
			guard let p = full(node) else { fail(ENOENT); break }
			guard let d = opendir(p) else { fail(errno); break }
			var l: [(name: String, ino: UInt64, type: UInt32)] = []
			while let e = readdir(d) {
				let name = withUnsafeBytes(of: e.pointee.d_name) { String(decoding: $0.prefix(Int(e.pointee.d_namlen)), as: UTF8.self) }
				l.append((name, e.pointee.d_ino, UInt32(e.pointee.d_type)))
			}
			closedir(d)
			dirs[nextDir] = l
			out.u64(nextDir); out.u32(0); out.u32(0)
			nextDir += 1
		case 15: // READ
			guard let fd = files[u64(40)] else { fail(EBADF); break }
			let size = min(Int(u32(56)), 1 << 20) // max_pages
			out.b = [UInt8](repeating: 0, count: size)
			let n = out.b.withUnsafeMutableBytes { pread(fd, $0.baseAddress!, size, off_t(bitPattern: u64(48))) }
			if n < 0 { fail(errno); out.b = [] } else { out.b.removeLast(size - n) }
		case 28: // READDIR
			guard let l = dirs[u64(40)] else { fail(EBADF); break }
			let size = Int(u32(56))
			var i = Int(clamping: u64(48))
			while i < l.count {
				let e = l[i], name = Array(e.name.utf8)
				let len = (24 + name.count + 7) & ~7
				if out.b.count + len > size { break }
				i += 1
				out.u64(e.ino); out.u64(UInt64(i)); out.u32(UInt32(name.count)); out.u32(e.type)
				out.b += name
				out.zeros(len - 24 - name.count)
			}
		case 18: if let fd = files.removeValue(forKey: u64(40)) { close(fd) } // RELEASE
		case 29: dirs[u64(40)] = nil // RELEASEDIR
		case 25: break // FLUSH
		case 38: mounted = false // DESTROY
		case 34: if u32(40) & 2 != 0 { fail(EROFS) } // ACCESS: W_OK
		case 17: // STATFS
			out.zeros(40); out.u32(4096); out.u32(255); out.u32(4096); out.zeros(28)
		default: fail(ENOSYS)
		}
		var r = Out()
		r.u32(UInt32(16 + (err == 0 ? out.b.count : 0))); r.u32(UInt32(bitPattern: err)); r.u64(unique)
		return r.b + (err == 0 ? out.b : [])
	}
}

// A FUSE reply's body.
struct Out {
	var b: [UInt8] = []
	mutating func u16(_ v: UInt16) { withUnsafeBytes(of: v) { b += $0 } }
	mutating func u32(_ v: UInt32) { withUnsafeBytes(of: v) { b += $0 } }
	mutating func u64(_ v: UInt64) { withUnsafeBytes(of: v) { b += $0 } }
	mutating func zeros(_ n: Int) { b += [UInt8](repeating: 0, count: n) }
	mutating func attr(_ st: stat) { // struct fuse_attr
		u64(st.st_ino); u64(UInt64(st.st_size)); u64(UInt64(st.st_blocks))
		u64(UInt64(bitPattern: Int64(st.st_atimespec.tv_sec))); u64(UInt64(bitPattern: Int64(st.st_mtimespec.tv_sec)))
		u64(UInt64(bitPattern: Int64(st.st_ctimespec.tv_sec)))
		u32(UInt32(st.st_atimespec.tv_nsec)); u32(UInt32(st.st_mtimespec.tv_nsec)); u32(UInt32(st.st_ctimespec.tv_nsec))
		u32(UInt32(st.st_mode)); u32(UInt32(st.st_nlink)); u32(st.st_uid); u32(st.st_gid); u32(0); u32(4096); u32(0)
	}
}
