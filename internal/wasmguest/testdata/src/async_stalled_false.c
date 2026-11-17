/* Synthetic host-adapter fixture. Not the QuickPy guest. */
#define EXPORT __attribute__((visibility("default")))
static int step = 0;
static const char *responses[] = {
    "{\"v\":0,\"type\":\"suspended\",\"reason\":\"stalled\",\"stalled\":false,\"calls\":[],\"awaiting\":[],\"output\":\"\"}",
    "{\"v\":0,\"type\":\"completed\",\"output\":\"unexpected\\n\"}"
};
EXPORT int qp_abi_version(void) { return 1; }
EXPORT int qp_alloc(int len) { (void)len; return 1024; }
EXPORT void qp_free(int ptr, int len) { (void)ptr; (void)len; }
EXPORT long long qp_invoke(int ptr, int len) {
    (void)ptr; (void)len;
    int idx = step < 1 ? step : 1;
    step++;
    const char *resp = responses[idx];
    unsigned n = 0; while (resp[n]) n++;
    volatile unsigned char *out = (volatile unsigned char *)2048;
    for (unsigned i = 0; i < n; i++) out[i] = (unsigned char)resp[i];
    return ((long long)2048 << 32) | (long long)n;
}
