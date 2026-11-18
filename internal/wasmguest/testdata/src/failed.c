/* Synthetic host-adapter fixture. Not the QuickPy guest. */
#define EXPORT __attribute__((visibility("default")))
static const char resp[] = "{\"v\":0,\"type\":\"failed\",\"output\":\"before failure\",\"error\":{\"type\":\"ValueError\",\"message\":\"boom\",\"traceback\":\"Traceback (most recent call last):\\n  File \\\"main.py\\\", line 3\\nValueError: boom\",\"filename\":\"main.py\",\"line\":3}}";
EXPORT int qp_abi_version(void) { return 2; }
EXPORT int qp_alloc(int len) { (void)len; return 1024; }
EXPORT void qp_free(int ptr, int len) { (void)ptr; (void)len; }
EXPORT long long qp_invoke(int ptr, int len) {
    (void)ptr; (void)len;
    unsigned n = sizeof(resp) - 1;
    volatile unsigned char *out = (volatile unsigned char *)2048;
    for (unsigned i = 0; i < n; i++) out[i] = (unsigned char)resp[i];
    return ((long long)2048 << 32) | (long long)n;
}
