// A flattened device tree (DTB) writer: just enough to describe the
// machine to the guest kernel.

struct FDT {
	private var structs: [UInt8] = []
	private var strings: [UInt8] = []
	private var offsets: [String: UInt32] = [:]

	private mutating func u32(_ v: UInt32) { withUnsafeBytes(of: v.bigEndian) { structs += $0 } }
	private mutating func pad() { while structs.count % 4 != 0 { structs.append(0) } }

	mutating func begin(_ name: String) {
		u32(1)
		structs += Array(name.utf8) + [0]
		pad()
	}

	mutating func end() { u32(2) }

	mutating func prop(_ name: String, _ data: [UInt8] = []) {
		let off: UInt32
		if let o = offsets[name] {
			off = o
		} else {
			off = UInt32(strings.count)
			offsets[name] = off
			strings += Array(name.utf8) + [0]
		}
		u32(3)
		u32(UInt32(data.count))
		u32(off)
		structs += data
		pad()
	}

	mutating func prop(_ name: String, u32s: [UInt32]) {
		prop(name, u32s.flatMap { v in withUnsafeBytes(of: v.bigEndian) { Array($0) } })
	}

	mutating func prop(_ name: String, u64s: [UInt64]) {
		prop(name, u64s.flatMap { v in withUnsafeBytes(of: v.bigEndian) { Array($0) } })
	}

	mutating func prop(_ name: String, strings l: [String]) {
		prop(name, l.flatMap { Array($0.utf8) + [0] })
	}

	func blob() -> [UInt8] {
		var s = structs
		withUnsafeBytes(of: UInt32(9).bigEndian) { s += $0 }
		let rsvOff = 40, structOff = rsvOff + 16, stringsOff = structOff + s.count
		let total = stringsOff + strings.count
		var h: [UInt8] = []
		for v in [0xd00d_feed, total, structOff, stringsOff, rsvOff, 17, 16, 0, strings.count, s.count] {
			withUnsafeBytes(of: UInt32(v).bigEndian) { h += $0 }
		}
		return h + [UInt8](repeating: 0, count: 16) + s + strings
	}
}
