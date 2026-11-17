/* Synthetic host-adapter fixture. Not the QuickPy guest. */
#define EXPORT __attribute__((visibility("default")))
__attribute__((import_module("wasi_snapshot_preview1"), import_name("path_open")))
extern int path_open(int, int, int, int, int, long long, int, int, int);
EXPORT int qp_abi_version(void) { return 1; }
EXPORT int qp_alloc(int len) { (void)len; return 1024; }
EXPORT void qp_free(int ptr, int len) { (void)ptr; (void)len; }
EXPORT long long qp_invoke(int ptr, int len) {
    (void)ptr; (void)len;
    int fd = -1;
    path_open(3, 0, 0, 0, 0, 0, 0, 0, (int)(long long)&fd);
    return 0;
}
