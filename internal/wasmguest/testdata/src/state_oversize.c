/* Synthetic host-adapter fixture. Not the QuickPy guest. */
#define EXPORT __attribute__((visibility("default")))
static const char prefix[] = "{\"v\":0,\"type\":\"completed\",\"output\":\"\",\"state_written\":true,\"state\":\"";
static const char suffix[] = "\"}";
EXPORT int qp_abi_version(void) { return 4; }
EXPORT int qp_alloc(int len) { (void)len; return 1024; }
EXPORT void qp_free(int ptr, int len) { (void)ptr; (void)len; }
EXPORT long long qp_invoke(int ptr, int len) {
    (void)ptr; (void)len;
    volatile unsigned char *out = (volatile unsigned char *)2048;
    unsigned n = 0;
    for (const char *p = prefix; *p; p++) out[n++] = (unsigned char)*p;
    for (int i = 0; i < 200; i++) out[n++] = (unsigned char)'a';
    for (const char *p = suffix; *p; p++) out[n++] = (unsigned char)*p;
    return ((long long)2048 << 32) | (long long)n;
}
