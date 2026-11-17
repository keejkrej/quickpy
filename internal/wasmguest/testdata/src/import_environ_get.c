/* Synthetic host-adapter fixture. Not the QuickPy guest. */
#define EXPORT __attribute__((visibility("default")))
__attribute__((import_module("wasi_snapshot_preview1"), import_name("environ_get")))
extern int environ_get(int, int);
EXPORT int qp_abi_version(void) { return 1; }
EXPORT int qp_alloc(int len) { (void)len; return 1024; }
EXPORT void qp_free(int ptr, int len) { (void)ptr; (void)len; }
EXPORT long long qp_invoke(int ptr, int len) {
    (void)ptr; (void)len;
    environ_get(0, 0);
    return 0;
}
