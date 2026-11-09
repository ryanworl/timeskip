// hvrun: run the time-skip guest on Hypervisor.framework (Apple silicon,
// macOS 15+). Run by timeskip. One vCPU, Hypervisor.framework's GICv3, a
// virtio-mmio console (hvc0), disks and entropy, PSCI over HVC, and stolen
// time (SMCCC's paravirtualized time).
//
// The difference is restore: "@save PATH" writes the machine state to PATH
// and guest memory to PATH.mem, and --restore PATH maps PATH.mem
// copy-on-write (MAP_PRIVATE) as guest memory, so a restored VM reads its
// pages from the page cache as it touches them, and VMs restored from the
// same file share them. A save of a restored VM clones PATH.mem and writes
// only the pages the VM changed.
//
// The guest's virtual counter stands still while the VM is paused or
// saved: a restored or resumed VM continues from the count it was saved
// at. Otherwise it runs, and the guest counts as stolen what the vCPU
// didn't run of it (see steal), as a KVM guest does its vCPU thread's waits
// for a CPU: not the task that ran's CPU time.
//
// --share TAG=DIR is a read-only virtio-fs device.
//
// Build: swiftc -O -o hvrun hv/*.swift && codesign -f -s - --entitlements hv/hv.entitlements hvrun
import Foundation
import Hypervisor

var kernel = "", cmdline = ""
var memMB: UInt64 = 4096
var disks: [(String, Bool)] = []
var shares: [(String, String)] = []
var restore: String?
var args = Array(CommandLine.arguments.dropFirst())
while !args.isEmpty {
	let a = args.removeFirst()
	switch a {
	case "--kernel": kernel = args.removeFirst()
	case "--cmdline": cmdline = args.removeFirst()
	case "--mem-mb": memMB = UInt64(args.removeFirst())!
	case "--disk": disks.append((args.removeFirst(), false))
	case "--disk-ro": disks.append((args.removeFirst(), true))
	case "--share":
		let a = args.removeFirst().split(separator: "=", maxSplits: 1).map(String.init)
		shares.append((a[0], a[1]))
	case "--restore": restore = args.removeFirst()
	default: fatalError("unknown arg \(a)")
	}
}

func report(_ s: String) { FileHandle.standardError.write("hvrun: \(s)\n".data(using: .utf8)!) }
func fail(_ s: String) -> Never { report(s); exit(1) }
func check(_ r: hv_return_t, _ what: @autoclosure () -> String) {
	if r != HV_SUCCESS { fail("\(what()): error 0x\(String(UInt32(bitPattern: r), radix: 16))") }
}

var timebase = mach_timebase_info_data_t()
mach_timebase_info(&timebase)


// The machine's physical layout (as QEMU's virt machine).
let gicdBase: UInt64 = 0x0800_0000, gicrBase: UInt64 = 0x080a_0000
let rtcBase: UInt64 = 0x0901_0000 // a PL031 real-time clock: the guest's time of day at boot
let virtioBase: UInt64 = 0x0a00_0000, virtioStride: UInt64 = 0x200
let stealBase: UInt64 = 0x0b00_0000 // the stolen time structure (SMCCC DEN0057A)
let ramBase: UInt64 = 0x4000_0000
let ramSize = memMB << 20
let page = Int(vm_page_size)

// A saved machine, besides its memory.
struct MachineState: Codable {
	var memMB: UInt64
	var regs: [UInt64] // X0-X30, PC, FPCR, FPSR, CPSR
	var simd: [Data] // Q0-Q31
	var sys: [[UInt64]] // [register, value]
	var icc: [[UInt64]]
	var cntvct: UInt64 // the virtual counter when saved
	var stolen: UInt64 // the stolen time, ns
	var gic: Data // hv_gic_state_get_data
	var devices: [DeviceState]
}

var saved: MachineState?
if let restore {
	guard let d = FileManager.default.contents(atPath: restore),
		let s = try? JSONDecoder().decode(MachineState.self, from: d) else { fail("can't read \(restore)") }
	if s.memMB != memMB { fail("\(restore) has \(s.memMB) MB of memory, not \(memMB)") }
	saved = s
}

// Guest memory: anonymous, or the saved memory mapped copy-on-write.
var memFile: String? // the file guest memory is a private mapping of
let ram: UnsafeMutableRawPointer = {
	var fd: Int32 = -1, flags = MAP_ANON | MAP_PRIVATE
	if let restore {
		memFile = restore + ".mem"
		fd = open(memFile!, O_RDONLY)
		if fd < 0 { fail("open \(memFile!): \(String(cString: strerror(errno)))") }
		flags = MAP_PRIVATE
	}
	let p = mmap(nil, Int(ramSize), PROT_READ | PROT_WRITE, flags, fd, 0)
	if p == UnsafeMutableRawPointer(bitPattern: -1) { fail("mmap: \(String(cString: strerror(errno)))") }
	if fd >= 0 { readahead(fd) }
	return p!
}()

// readahead reads a memory file's data (not its holes) into the page cache
// in the background, so that the guest's first touches of its pages don't
// each wait for the disk (as they would on a newly cloned file).
func readahead(_ fd: Int32) {
	Thread {
		var off: off_t = 0
		while true {
			let data = lseek(fd, off, SEEK_DATA)
			if data < 0 { break }
			let hole = lseek(fd, data, SEEK_HOLE)
			var ra = radvisory(ra_offset: data, ra_count: Int32(min(hole - data, 1 << 30)))
			_ = fcntl(fd, F_RDADVISE, &ra)
			off = data + off_t(ra.ra_count)
		}
		close(fd)
	}.start()
}
mem = Mem(base: ramBase, size: ramSize, host: ram)

check(hv_vm_create(nil), "hv_vm_create")
check(hv_vm_map(ram, ramBase, Int(ramSize), hv_memory_flags_t(HV_MEMORY_READ | HV_MEMORY_WRITE | HV_MEMORY_EXEC)), "hv_vm_map")
// (Its revision and attributes, at 0 and 4, are 0.)
let stealPage = mmap(nil, page, PROT_READ | PROT_WRITE, MAP_ANON | MAP_PRIVATE, -1, 0)!
check(hv_vm_map(stealPage, stealBase, page, hv_memory_flags_t(HV_MEMORY_READ | HV_MEMORY_WRITE)), "hv_vm_map")

// steal adds ns to the stolen time: the time the guest's counter ran and its
// vCPU didn't, while the host ran other threads, or this one handled an exit
// (a disk's I/O, say: the guest would otherwise count the time as the CPU
// time of the task that wrote to the device). Not while it waits in WFI,
// which is the guest's idle time.
var stolen: UInt64 = 0
func steal(_ ns: UInt64) {
	stolen &+= ns
	stealPage.storeBytes(of: stolen.littleEndian, toByteOffset: 8, as: UInt64.self)
}
func now() -> UInt64 { clock_gettime_nsec_np(CLOCK_UPTIME_RAW) }

let gicConfig = hv_gic_config_create()
check(hv_gic_config_set_distributor_base(gicConfig, gicdBase), "GIC distributor base")
check(hv_gic_config_set_redistributor_base(gicConfig, gicrBase), "GIC redistributor base")
check(hv_gic_create(gicConfig), "hv_gic_create")
var gicdSize = 0, gicrSize = 0
hv_gic_get_distributor_size(&gicdSize)
hv_gic_get_redistributor_size(&gicrSize)
var spiBase: UInt32 = 0, spiCount: UInt32 = 0
hv_gic_get_spi_interrupt_range(&spiBase, &spiCount)

// Devices, in guest order: hvc0, then vda, vdb, ..., entropy and shares.
let console = Console()
var devs: [Transport] = []
func add(_ d: VirtioDevice) { devs.append(Transport(d, spi: spiBase + UInt32(devs.count))) }
add(console)
for (path, ro) in disks { add(Blk(path, ro: ro)) }
add(Rng())
for (tag, dir) in shares { add(FS(tag: tag, root: dir)) }

// The vCPU belongs to this thread, which runs it (on a performance core).
pthread_set_qos_class_self_np(QOS_CLASS_USER_INTERACTIVE, 0)
var vcpu: hv_vcpu_t = 0
var exitInfo: UnsafeMutablePointer<hv_vcpu_exit_t>?
check(hv_vcpu_create(&vcpu, &exitInfo, nil), "hv_vcpu_create")

func reg(_ r: Int) -> UInt64 {
	var v: UInt64 = 0
	check(hv_vcpu_get_reg(vcpu, hv_reg_t(rawValue: UInt32(r)), &v), "get register \(r)")
	return v
}
func setReg(_ r: Int, _ v: UInt64) { check(hv_vcpu_set_reg(vcpu, hv_reg_t(rawValue: UInt32(r)), v), "set register \(r)") }
func sysReg(_ r: UInt16) -> UInt64 {
	var v: UInt64 = 0
	check(hv_vcpu_get_sys_reg(vcpu, hv_sys_reg_t(rawValue: r), &v), "get system register \(r)")
	return v
}
func setSysReg(_ r: UInt16, _ v: UInt64) { check(hv_vcpu_set_sys_reg(vcpu, hv_sys_reg_t(rawValue: r), v), "set system register \(r)") }
let PC = 31, CPSR = 34
let MPIDR: UInt16 = 0xc005, CNTV_CTL: UInt16 = 0xdf19, CNTV_CVAL: UInt16 = 0xdf1a

setSysReg(MPIDR, 0x8000_0000)

// CNTVCT_EL0 = mach_absolute_time() - vtOffset
var vtOffset: UInt64 = 0
func setCounter(_ cntvct: UInt64) {
	vtOffset = mach_absolute_time() &- cntvct
	check(hv_vcpu_set_vtimer_offset(vcpu, vtOffset), "hv_vcpu_set_vtimer_offset")
}
func counter() -> UInt64 { mach_absolute_time() &- vtOffset }

// The vCPU state saved, besides X0-X30, PC, FPCR, FPSR and CPSR.
let sysRegs: [UInt16] = (0..<16).flatMap { n in (0..<4).map { UInt16(0x8004 + 8 * n + $0) } } + [
	0x8010, 0x8012, // MDCCINT, MDSCR
	0xc005, 0xc080, 0xc081, 0xc082, // MPIDR, SCTLR, ACTLR, CPACR
	0xc100, 0xc101, 0xc102, // TTBR0, TTBR1, TCR
	0xc200, 0xc201, 0xc208, 0xc288, 0xc289, 0xc290, 0xc300, 0xc3a0, // SPSR, ELR, SP_EL0, AFSR0/1, ESR, FAR, PAR
	0xc510, 0xc518, 0xc600, 0xc681, 0xc684, 0xc708, 0xd000, // MAIR, AMAIR, VBAR, CONTEXTIDR, TPIDR_EL1, CNTKCTL, CSSELR
	0xde82, 0xde83, 0xe208, // TPIDR_EL0, TPIDRRO_EL0, SP_EL1
	CNTV_CTL, CNTV_CVAL, 0xdf11, 0xdf12, // CNTV_*, CNTP_CTL, CNTP_CVAL
]
let iccRegs: [UInt16] = [0xc230, 0xc643, 0xc644, 0xc648, 0xc663, 0xc664, 0xc665, 0xc666, 0xc667]

func dtb() -> [UInt8] {
	var f = FDT()
	f.begin("")
	f.prop("compatible", strings: ["linux,dummy-virt"])
	f.prop("#address-cells", u32s: [2])
	f.prop("#size-cells", u32s: [2])
	f.prop("interrupt-parent", u32s: [1])
	f.begin("chosen")
	f.prop("bootargs", strings: [cmdline])
	var seed = [UInt8](repeating: 0, count: 64)
	arc4random_buf(&seed, seed.count)
	f.prop("rng-seed", seed)
	f.end()
	f.begin("memory@" + String(ramBase, radix: 16))
	f.prop("device_type", strings: ["memory"])
	f.prop("reg", u64s: [ramBase, ramSize])
	f.end()
	f.begin("cpus")
	f.prop("#address-cells", u32s: [1])
	f.prop("#size-cells", u32s: [0])
	f.begin("cpu@0")
	f.prop("device_type", strings: ["cpu"])
	f.prop("compatible", strings: ["arm,arm-v8"])
	f.prop("reg", u32s: [0])
	f.prop("enable-method", strings: ["psci"])
	f.end()
	f.end()
	f.begin("psci")
	f.prop("compatible", strings: ["arm,psci-1.0", "arm,psci-0.2"])
	f.prop("method", strings: ["hvc"])
	f.end()
	f.begin("intc@" + String(gicdBase, radix: 16))
	f.prop("compatible", strings: ["arm,gic-v3"])
	f.prop("#interrupt-cells", u32s: [3])
	f.prop("interrupt-controller")
	f.prop("reg", u64s: [gicdBase, UInt64(gicdSize), gicrBase, UInt64(gicrSize)])
	f.prop("phandle", u32s: [1])
	f.end()
	f.begin("apb-pclk")
	f.prop("compatible", strings: ["fixed-clock"])
	f.prop("#clock-cells", u32s: [0])
	f.prop("clock-frequency", u32s: [24_000_000])
	f.prop("phandle", u32s: [2])
	f.end()
	f.begin("pl031@" + String(rtcBase, radix: 16))
	f.prop("compatible", strings: ["arm,pl031", "arm,primecell"])
	f.prop("reg", u64s: [rtcBase, 0x1000])
	f.prop("clocks", u32s: [2])
	f.prop("clock-names", strings: ["apb_pclk"])
	f.end()
	f.begin("timer")
	f.prop("compatible", strings: ["arm,armv8-timer"])
	f.prop("interrupts", u32s: [1, 13, 4, 1, 14, 4, 1, 11, 4, 1, 10, 4])
	f.prop("always-on")
	f.end()
	for (i, d) in devs.enumerated() {
		let base = virtioBase + UInt64(i) * virtioStride
		f.begin("virtio_mmio@" + String(base, radix: 16))
		f.prop("compatible", strings: ["virtio,mmio"])
		f.prop("reg", u64s: [base, virtioStride])
		f.prop("interrupts", u32s: [0, d.spi - 32, 4])
		f.prop("dma-coherent")
		f.end()
	}
	f.end()
	return f.blob()
}

// load reads a file into guest memory at a.
func load(_ path: String, at a: UInt64) {
	let fd = open(path, O_RDONLY)
	if fd < 0 { fail("open \(path): \(String(cString: strerror(errno)))") }
	defer { close(fd) }
	var st = stat()
	fstat(fd, &st)
	let n = Int(st.st_size)
	var done = 0
	while done < n {
		let r = read(fd, mem.ptr(a + UInt64(done), n - done), n - done)
		if r <= 0 { fail("read \(path)") }
		done += r
	}
}

func boot() {
	// The arm64 Image header: text_offset at 8.
	guard let h = FileHandle(forReadingAtPath: kernel)?.readData(ofLength: 64), h.count == 64 else { fail("can't read \(kernel)") }
	let textOffset = h.withUnsafeBytes { $0.loadUnaligned(fromByteOffset: 8, as: UInt64.self) }
	let entry = ramBase + textOffset
	load(kernel, at: entry)
	let mb2: UInt64 = 2 << 20
	let d = dtb()
	let dtbAt = ramBase + ramSize - mb2
	d.withUnsafeBytes { mem.ptr(dtbAt, d.count).copyMemory(from: $0.baseAddress!, byteCount: d.count) }
	setReg(PC, entry)
	setReg(0, dtbAt)
	setReg(CPSR, 0x3c5) // EL1h, interrupts masked
	setCounter(0)
}

func restoreMachine(_ s: MachineState) {
	s.gic.withUnsafeBytes { check(hv_gic_set_state($0.baseAddress!, s.gic.count), "hv_gic_set_state") }
	setCounter(s.cntvct) // (before the timer's registers, which compare against it)
	for (i, v) in s.regs.enumerated() { setReg(i, v) }
	for (i, d) in s.simd.enumerated() {
		var v = hv_simd_fp_uchar16_t()
		withUnsafeMutableBytes(of: &v) { d.copyBytes(to: $0) }
		check(hv_vcpu_set_simd_fp_reg(vcpu, hv_simd_fp_reg_t(rawValue: UInt32(i)), v), "set Q\(i)")
	}
	for r in s.sys { setSysReg(UInt16(r[0]), r[1]) }
	for r in s.icc { check(hv_gic_set_icc_reg(vcpu, hv_gic_icc_reg_t(rawValue: UInt16(r[0])), r[1]), "set ICC register \(r[0])") }
	if s.devices.count != devs.count { fail("the saved machine has \(s.devices.count) devices, not \(devs.count)") }
	for (t, d) in zip(devs, s.devices) { t.restore(d) }
	stolen = s.stolen
	steal(0)
}

func save(_ path: String, cntvct: UInt64) {
	if devs.contains(where: { ($0.dev as? FS)?.mounted == true }) { fail("@save: a share is mounted") }
	let gicState = hv_gic_state_create()
	var gicSize = 0
	check(hv_gic_state_get_size(gicState, &gicSize), "hv_gic_state_get_size")
	var gic = Data(count: gicSize)
	gic.withUnsafeMutableBytes { check(hv_gic_state_get_data(gicState, $0.baseAddress!), "hv_gic_state_get_data") }
	var sys: [[UInt64]] = []
	for r in sysRegs {
		var v: UInt64 = 0
		if hv_vcpu_get_sys_reg(vcpu, hv_sys_reg_t(rawValue: r), &v) == HV_SUCCESS { sys.append([UInt64(r), v]) }
	}
	let s = MachineState(
		memMB: memMB,
		regs: (0...34).map(reg),
		simd: (0..<32).map { i in
			var v = hv_simd_fp_uchar16_t()
			check(hv_vcpu_get_simd_fp_reg(vcpu, hv_simd_fp_reg_t(rawValue: UInt32(i)), &v), "get Q\(i)")
			return withUnsafeBytes(of: v) { Data($0) }
		},
		sys: sys,
		icc: iccRegs.map { r in
			var v: UInt64 = 0
			check(hv_gic_get_icc_reg(vcpu, hv_gic_icc_reg_t(rawValue: r), &v), "get ICC register \(r)")
			return [UInt64(r), v]
		},
		cntvct: cntvct, stolen: stolen, gic: gic, devices: devs.map(\.state))
	do {
		try JSONEncoder().encode(s).write(to: URL(fileURLWithPath: path))
	} catch { fail("save failed: \(error)") }
	saveMemory(path + ".mem")
}

// saveMemory writes guest memory to dst, sparse. Booted, that's the pages
// the guest touched that aren't zero; restored, a clone of the file it was
// restored from with the pages it changed (copied on write) written over,
// or made holes where they're zero now (the guest zeroes pages it frees
// during setup: written, they'd take as much room as data).
func saveMemory(_ dst: String) {
	let n = Int(ramSize) / page
	var info = [CChar](repeating: 0, count: n)
	if mincore(ram, Int(ramSize), &info) != 0 { fail("mincore: \(String(cString: strerror(errno)))") }
	unlink(dst)
	if let src = memFile, clonefile(src, dst, 0) != 0 { fail("clonefile \(src): \(String(cString: strerror(errno)))") }
	let fd = open(dst, O_WRONLY | O_CREAT, 0o644)
	if fd < 0 || ftruncate(fd, off_t(ramSize)) != 0 { fail("can't write \(dst)") }
	defer { close(fd) }
	let zero = UnsafeMutableRawPointer.allocate(byteCount: page, alignment: page)
	zero.initializeMemory(as: UInt8.self, repeating: 0, count: page)
	defer { zero.deallocate() }
	// Each page's fate: 0 left as it is (a hole, booted; the clone's,
	// restored), 1 written, 2 made a hole. Booted, a page the guest touched
	// that isn't zero is written; restored, one in the mapping's own
	// (anonymous) copy, in memory or compressed, is written or, if zero, made
	// a hole.
	let fate: [Int] = (0..<n).map { i in
		let f = Int32(info[i])
		let touched = memFile != nil ? f & (MINCORE_COPIED | MINCORE_ANONYMOUS) != 0 : f & (MINCORE_INCORE | MINCORE_PAGED_OUT) != 0
		if !touched { return 0 }
		if memcmp(ram + i * page, zero, page) != 0 { return 1 }
		return memFile != nil ? 2 : 0
	}
	var i = 0
	while i < n {
		var j = i + 1
		while j < n && fate[j] == fate[i] && (fate[i] != 1 || j - i < 256) { j += 1 }
		if fate[i] == 1, pwrite(fd, ram + i * page, (j - i) * page, off_t(i * page)) != (j - i) * page {
			fail("write \(dst): \(String(cString: strerror(errno)))")
		}
		if fate[i] == 2 {
			var h = fpunchhole_t(fp_flags: 0, reserved: 0, fp_offset: off_t(i * page), fp_length: off_t((j - i) * page))
			if fcntl(fd, F_PUNCHHOLE, &h) != 0 { fail("punching a hole in \(dst): \(String(cString: strerror(errno)))") }
		}
		i = j
	}
}

if let s = saved { restoreMachine(s) } else { boot() }

// Control: stdin lines, read on their own thread, become requests for the
// vCPU thread, which hv_vcpus_exit (or wake, in WFI) gets its attention.
// hv_vcpus_exit on a vCPU that isn't in hv_vcpu_run makes its next
// hv_vcpu_run return at once (hv_vcpu.h), so a request queued after
// service() and before hv_vcpu_run still ends the run.
enum Request { case save(String), resume }
let lock = NSLock()
var requests: [Request] = []
let wake = DispatchSemaphore(value: 0)
func kick() {
	var v = vcpu
	hv_vcpus_exit(&v, 1)
	wake.signal()
}
Thread {
	var buf = [UInt8](repeating: 0, count: 1 << 16), line: [UInt8] = []
	while true {
		let n = read(0, &buf, buf.count)
		if n <= 0 { exit(0) }
		for b in buf[0..<n] {
			if b != 0x0a {
				line.append(b)
				continue
			}
			let s = String(decoding: line, as: UTF8.self)
			lock.lock()
			if s.hasPrefix("@save ") {
				requests.append(.save(String(s.dropFirst(6))))
			} else if s == "@resume" {
				requests.append(.resume)
			} else {
				console.input(line + [0x0a])
			}
			lock.unlock()
			line.removeAll()
			kick()
		}
	}
}.start()

var paused = false, pausedAt: UInt64 = 0
func service() {
	while true {
		lock.lock()
		let rs = requests
		requests.removeAll()
		lock.unlock()
		for r in rs {
			switch r {
			case .save(let path):
				if !paused { pausedAt = counter() }
				paused = true
				save(path, cntvct: pausedAt)
				report("saved")
			case .resume:
				if paused { setCounter(pausedAt) }
				paused = false
			}
		}
		if !paused { break }
		wake.wait()
	}
	console.deliver(devs[0])
}

// wfi waits until the guest has an interrupt, its timer fires, or a request.
// (Hypervisor.framework's GIC handles WFI in the host kernel; this is for a
// WFI exit, should one come.)
func wfi() {
	if devs.contains(where: { $0.line }) { return }
	let ctl = sysReg(CNTV_CTL)
	var deadline = DispatchTime.distantFuture
	if ctl & 3 == 1 { // enabled, not masked
		let cval = sysReg(CNTV_CVAL), now = counter()
		if cval <= now { return }
		let ns = Double(cval - now) * Double(timebase.numer) / Double(timebase.denom)
		deadline = .now() + .nanoseconds(Int(min(ns, 3.6e12)))
	}
	steal(now() - exited)
	_ = wake.wait(timeout: deadline)
	exited = now()
}

let notSupported = UInt64(bitPattern: -1)
func psci() {
	var r = notSupported
	switch UInt32(truncatingIfNeeded: reg(0)) {
	case 0x8400_0000: r = 0x1_0000 // PSCI_VERSION: 1.0
	case 0x8400_000a: if reg(1) == 0x8000_0000 { r = 0 } // PSCI_FEATURES: SMCCC_VERSION
	case 0x8400_0008, 0x8400_0009: exit(0) // SYSTEM_OFF, SYSTEM_RESET
	case 0x8000_0000: r = 0x1_0001 // SMCCC_VERSION: 1.1
	case 0x8000_0001: if reg(1) == 0xc500_0020 { r = 0 } // SMCCC_ARCH_FEATURES: PV_TIME_FEATURES
	case 0xc500_0020: if reg(1) == 0xc500_0021 { r = 0 } // PV_TIME_FEATURES: PV_TIME_ST
	case 0xc500_0021: r = stealBase // PV_TIME_ST
	default: break
	}
	setReg(0, r)
}

func mmio(_ pa: UInt64, _ len: Int, write v: UInt64?) -> UInt64 {
	if pa >= virtioBase && pa < virtioBase + virtioStride * UInt64(devs.count) {
		let d = devs[Int((pa - virtioBase) / virtioStride)], off = (pa - virtioBase) % virtioStride
		if let v { d.write(off, v) } else { return d.read(off, len) }
		return 0
	}
	if pa >= rtcBase && pa < rtcBase + 0x1000 && v == nil {
		switch pa - rtcBase {
		case 0x000: return UInt64(time(nil)) // RTCDR
		case 0x00c: return 1 // RTCCR: started
		case 0xfe0...0xffc: return [0x31, 0x10, 0x04, 0x00, 0x0d, 0xf0, 0x05, 0xb1][Int(pa - rtcBase - 0xfe0) / 4] // PrimeCell IDs
		default: return 0
		}
	}
	return 0
}

// The counter again, now the guest is about to run: the rest of the restore
// took host time the guest mustn't see.
if let s = saved { setCounter(s.cntvct) }

// exited is when the vCPU last stopped running the guest (or a wait the
// guest's counter didn't run through, or WFI's, ended).
var exited = now()
while true {
	steal(now() - exited)
	service()
	let w = now(), c = clock_gettime_nsec_np(CLOCK_THREAD_CPUTIME_ID)
	check(hv_vcpu_run(vcpu), "hv_vcpu_run")
	exited = now()
	steal((exited - w) - min(exited - w, clock_gettime_nsec_np(CLOCK_THREAD_CPUTIME_ID) - c))
	let e = exitInfo!.pointee
	switch e.reason {
	case HV_EXIT_REASON_CANCELED: continue
	case HV_EXIT_REASON_VTIMER_ACTIVATED:
		// (Not with Hypervisor.framework's GIC, which takes the timer's interrupt.)
		check(hv_vcpu_set_vtimer_mask(vcpu, false), "hv_vcpu_set_vtimer_mask")
		continue
	case HV_EXIT_REASON_EXCEPTION: break
	default: fail("exit reason \(e.reason)")
	}
	let esr = e.exception.syndrome, iss = esr & 0x1ff_ffff
	var advance = true
	switch esr >> 26 & 0x3f {
	case 0x01: if iss & 1 == 0 { wfi() } // WFI (WFE: continue)
	case 0x16: psci(); advance = false // HVC (the PC is already past it)
	case 0x18: // a trapped system register: reads as zero, ignores writes
		let rt = Int(iss >> 5 & 0x1f)
		if iss & 1 == 1 && rt != 31 { setReg(rt, 0) }
	case 0x24: // data abort: MMIO
		guard iss >> 24 & 1 == 1 else { fail(String(format: "MMIO at 0x%llx without a syndrome", e.exception.physical_address)) }
		let len = 1 << Int(iss >> 22 & 3), rt = Int(iss >> 16 & 0x1f)
		if iss >> 6 & 1 == 1 {
			_ = mmio(e.exception.physical_address, len, write: rt == 31 ? 0 : reg(rt))
		} else {
			let v = mmio(e.exception.physical_address, len, write: nil)
			if rt != 31 { setReg(rt, v) }
		}
	default:
		fail(String(format: "unhandled exception: ESR 0x%llx, PC 0x%llx, address 0x%llx", esr, reg(PC), e.exception.physical_address))
	}
	if advance { setReg(PC, reg(PC) + 4) }
}
