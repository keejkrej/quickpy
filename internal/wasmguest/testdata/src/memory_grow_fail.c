/* Synthetic host-adapter fixture. Not the QuickPy guest. */
#define EXPORT __attribute__((visibility("default")))
static const char resp[] = "{\"v\":0,\"type\":\"failed\",\"error\":{\"type\":\"MemoryError\",\"message\":\"memory allocation failed\"}}";
EXPORT int qp_abi_version(void) { return 4; }
EXPORT int qp_alloc(int len) { (void)len; return 1024; }
EXPORT void qp_free(int ptr, int len) { (void)ptr; (void)len; }
EXPORT long long qp_invoke(int ptr, int len) {
    (void)ptr; (void)len;
    /* wazero returns -1 for a memory.grow beyond the configured cap; it does
       not trap. The guest then reports a MemoryError, which the host must not
       silently reclassify as LimitExceeded. */
    int grew = __builtin_wasm_memory_grow(0, 100000);
    (void)grew;
    unsigned n = sizeof(resp) - 1;
    volatile unsigned char *out = (volatile unsigned char *)2048;
    for (unsigned i = 0; i < n; i++) out[i] = (unsigned char)resp[i];
    return ((long long)2048 << 32) | (long long)n;
}
