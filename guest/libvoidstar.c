// libvoidstar.so: timeskip's stand-in for the library Antithesis injects
// into every container at /usr/lib/libvoidstar.so, through which the
// Antithesis SDKs send their output and get random numbers. Each SDK
// message is appended to the container's $ANTITHESIS_OUTPUT_DIR/sdk.jsonl
// (/timeskip-output, see cmd/guest), and random numbers come from the
// kernel, which the timeline's seed controls. Coverage instrumentation's
// callbacks (init_coverage_module, notify_coverage and ABI v2's
// notify_coverage_v2) set bits in files the guest agent reads
// (/timeskip-coverage, see cmd/guest/coverage.go), and when exploring, tell
// it of edges new to the VM.
//
// It has no libc (only system calls), so that it loads into any container,
// glibc's or musl's: cc -O2 -fPIC -shared -nostdlib -ffreestanding
// -fno-stack-protector -Wl,-z,defs (and on arm64 -mno-outline-atomics).

#include <stddef.h>
#include <stdint.h>

#if defined(__x86_64__)
enum { SYS_writev = 20, SYS_close = 3, SYS_openat = 257, SYS_getrandom = 318, SYS_mmap = 9, SYS_ftruncate = 77, SYS_fchmod = 91 };
static long sys6(long n, long a, long b, long c, long d, long e, long f) {
	register long r10 __asm__("r10") = d;
	register long r8 __asm__("r8") = e;
	register long r9 __asm__("r9") = f;
	long r;
	__asm__ volatile("syscall" : "=a"(r) : "a"(n), "D"(a), "S"(b), "d"(c), "r"(r10), "r"(r8), "r"(r9) : "rcx", "r11", "memory");
	return r;
}
#elif defined(__aarch64__)
enum { SYS_writev = 66, SYS_close = 57, SYS_openat = 56, SYS_getrandom = 278, SYS_mmap = 222, SYS_ftruncate = 46, SYS_fchmod = 52 };
static long sys6(long n, long a, long b, long c, long d, long e, long f) {
	register long x8 __asm__("x8") = n;
	register long x0 __asm__("x0") = a;
	register long x1 __asm__("x1") = b;
	register long x2 __asm__("x2") = c;
	register long x3 __asm__("x3") = d;
	register long x4 __asm__("x4") = e;
	register long x5 __asm__("x5") = f;
	__asm__ volatile("svc #0" : "+r"(x0) : "r"(x8), "r"(x1), "r"(x2), "r"(x3), "r"(x4), "r"(x5) : "memory");
	return x0;
}
#else
#error "x86_64 or aarch64 only"
#endif

static long sys4(long n, long a, long b, long c, long d) { return sys6(n, a, b, c, d, 0, 0); }

enum { AT_FDCWD = -100, O_WRONLY = 01, O_RDWR = 02, O_CREAT = 0100, O_APPEND = 02000, O_NONBLOCK = 04000, O_CLOEXEC = 02000000 };
enum { PROT_READ = 1, PROT_WRITE = 2, MAP_SHARED = 1 };

struct iovec {
	const void *base;
	size_t len;
};

// fuzz_json_data appends a message as a line, in one write (atomic with
// other appends).
void fuzz_json_data(const char *data, size_t size) {
	long fd = sys4(SYS_openat, AT_FDCWD, (long)"/timeskip-output/sdk.jsonl", O_WRONLY | O_CREAT | O_APPEND | O_CLOEXEC, 0666);
	if (fd < 0) {
		return;
	}
	struct iovec v[2] = {{data, size}, {"\n", 1}};
	sys4(SYS_writev, fd, (long)v, 2, 0);
	sys4(SYS_close, fd, 0, 0, 0);
}

void fuzz_flush(void) {}

uint64_t fuzz_get_random(void) {
	uint64_t r = 0;
	for (size_t got = 0; got < sizeof r;) {
		long n = sys4(SYS_getrandom, (long)((char *)&r + got), sizeof r - got, 0, 0);
		if (n > 0) {
			got += n;
		} else if (n != -4) { // (EINTR: again)
			break;
		}
	}
	return r;
}

// Coverage. Each instrumented module (by the name of its symbol table, as
// in /symbols) has a file in /timeskip-coverage: its number of edges (a
// uint64), then a bit per edge, 0 to edges (instrumentors number them from
// 1), which every process with the module maps, shared, and sets. The
// offsets are the process's own: notify_coverage gets an edge plus its
// module's offset.
enum { MAX_MODULES = 64 };
static struct {
	size_t offset, edges;
	uint64_t *bits; // (set last: the module is ready)
} modules[MAX_MODULES];
static size_t nmodules, next_offset;

size_t init_coverage_module(size_t edges, const char *symbols) {
	size_t i = __atomic_fetch_add(&nmodules, 1, __ATOMIC_SEQ_CST);
	size_t offset = __atomic_fetch_add(&next_offset, edges + 1, __ATOMIC_SEQ_CST);
	if (i >= MAX_MODULES) {
		return offset;
	}
	char path[256] = "/timeskip-coverage/";
	size_t n = 19;
	for (const char *c = symbols; *c && n < sizeof path - 1; c++) {
		int ok = (*c >= 'a' && *c <= 'z') || (*c >= 'A' && *c <= 'Z') || (*c >= '0' && *c <= '9') || *c == '.' || *c == '-' || *c == '_';
		path[n++] = ok ? *c : '_';
	}
	path[n] = 0;
	long fd = sys4(SYS_openat, AT_FDCWD, (long)path, O_RDWR | O_CREAT | O_CLOEXEC, 0666);
	if (fd < 0) {
		return offset;
	}
	sys4(SYS_fchmod, fd, 0666, 0, 0); // (past the umask, for other users' processes; only its creator can)
	size_t size = 8 + (edges / 64 + 1) * 8;
	long p = -1;
	if (sys4(SYS_ftruncate, fd, (long)size, 0, 0) == 0) {
		p = sys6(SYS_mmap, 0, (long)size, PROT_READ | PROT_WRITE, MAP_SHARED, fd, 0);
	}
	sys4(SYS_close, fd, 0, 0, 0);
	if ((unsigned long)p > -4096UL) {
		return offset;
	}
	uint64_t *h = (uint64_t *)p;
	h[0] = edges;
	modules[i].offset = offset;
	modules[i].edges = edges;
	__atomic_store_n(&modules[i].bits, h + 1, __ATOMIC_RELEASE);
	return offset;
}

// reach sets the edge's bit, and if no process had, writes a byte to the
// FIFO /timeskip-coverage/.new, if the guest agent has it open (when
// exploring).
static void reach(size_t edge) {
	size_t n = __atomic_load_n(&nmodules, __ATOMIC_ACQUIRE);
	for (size_t i = 0; i < n && i < MAX_MODULES; i++) {
		uint64_t *bits = __atomic_load_n(&modules[i].bits, __ATOMIC_ACQUIRE);
		if (bits && edge >= modules[i].offset && edge - modules[i].offset <= modules[i].edges) {
			size_t e = edge - modules[i].offset;
			uint64_t bit = (uint64_t)1 << (e % 64);
			if (!(__atomic_fetch_or(&bits[e / 64], bit, __ATOMIC_RELAXED) & bit)) {
				long fd = sys4(SYS_openat, AT_FDCWD, (long)"/timeskip-coverage/.new", O_WRONLY | O_NONBLOCK | O_CLOEXEC, 0);
				if (fd >= 0) {
					struct iovec v = {"", 1};
					sys4(SYS_writev, fd, (long)&v, 1, 0);
					sys4(SYS_close, fd, 0, 0, 0);
				}
			}
			break;
		}
	}
}

// notify_coverage is instrumentation ABI v1's callback, called with each
// hit of an edge until it returns false: the first is all reach needs.
_Bool notify_coverage(size_t edge) {
	reach(edge);
	return 0;
}

// ABI v2 (which the Go SDK uses from 0.8.0 on, and otherwise calls
// notify_coverage with every hit): notify_coverage_v2 gets an edge's hit
// and the hits counted since its last call, and returns a lease, the hits
// the caller may count without calling: an epoch (24 bits, the word at
// coverage_lease_generation_addr's, which never changes here), then the
// hits granted and those remaining (20 bits each), all it can grant.
static uint64_t lease_generation;

uint64_t instrumentation_request_abi_version(uint64_t v) { return v < 2 ? v : 2; }

const uint64_t *coverage_lease_generation_addr(void) { return &lease_generation; }

uint64_t notify_coverage_v2(size_t edge, uint64_t hits) {
	(void)hits;
	reach(edge);
	return (uint64_t)0xFFFFF << 20 | 0xFFFFF;
}
