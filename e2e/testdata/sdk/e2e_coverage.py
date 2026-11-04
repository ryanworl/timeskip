# Coverage as instrumented code reports it, through libvoidstar's
# callbacks: a module of 8 edges (e2e-cov.sym.tsv, in /symbols), and others,
# through ABI v1's notify_coverage or, with v2, notify_coverage_v2, which
# returns a lease (lease() says whether it granted hits).
import ctypes

lib = ctypes.CDLL("/usr/lib/libvoidstar.so")
lib.init_coverage_module.restype = ctypes.c_size_t
lib.init_coverage_module.argtypes = [ctypes.c_size_t, ctypes.c_char_p]
lib.notify_coverage.argtypes = [ctypes.c_size_t]
lib.instrumentation_request_abi_version.restype = ctypes.c_uint64
lib.instrumentation_request_abi_version.argtypes = [ctypes.c_uint64]
lib.notify_coverage_v2.restype = ctypes.c_uint64
lib.notify_coverage_v2.argtypes = [ctypes.c_size_t, ctypes.c_uint64]
leases = []


def module(symbols, edges, v2=False):
    """The function that reaches the module's edges, 1 to edges."""
    offset = lib.init_coverage_module(edges, symbols)
    if v2 and lib.instrumentation_request_abi_version(2) == 2:
        return lambda edge: leases.append(lib.notify_coverage_v2(offset + edge, 0))
    return lambda edge: lib.notify_coverage(offset + edge)


def lease():
    """Whether every v2 call so far granted hits (20 bits from bit 20)."""
    return len(leases) > 0 and all((w >> 20) & 0xFFFFF > 0 for w in leases)


reach = module(b"e2e-cov.sym.tsv", 8)
