/* Synthetic host-adapter fixture. Not the QuickPy guest. */
#include <stdint.h>
#define EXPORT __attribute__((visibility("default")))
static int step = 0;
static const char completed_resp[] = "{\"v\":0,\"type\":\"completed\",\"output\":\"unaffected\\n\"}";
static const char suspended_resp[] = "{\"v\":0,\"type\":\"suspended\",\"reason\":\"awaiting_calls\",\"calls\":[{\"id\":1,\"name\":\"block\",\"args\":{\"value\":\"X\",\"delay_ms\":10000}}],\"awaiting\":[1],\"output\":\"\"}";
static const char blocked_done_resp[] = "{\"v\":0,\"type\":\"completed\",\"output\":\"blocked done\\n\"}";

static int contains(const char *s, int len, const char *needle) {
    int n = 0;
    while (needle[n]) n++;
    for (int i = 0; i + n <= len; i++) {
        int j = 0;
        while (j < n && s[i + j] == needle[j]) j++;
        if (j == n) return 1;
    }
    return 0;
}

static long long respond(const char *resp) {
    unsigned n = 0;
    while (resp[n]) n++;
    volatile unsigned char *out = (volatile unsigned char *)2048;
    for (unsigned i = 0; i < n; i++) out[i] = (unsigned char)resp[i];
    return ((long long)2048 << 32) | (long long)n;
}

EXPORT int qp_abi_version(void) { return 4; }
EXPORT int qp_alloc(int len) { (void)len; return 1024; }
EXPORT void qp_free(int ptr, int len) { (void)ptr; (void)len; }
EXPORT long long qp_invoke(int ptr, int len) {
    const char *input = (const char *)(uintptr_t)(uint32_t)ptr;
    if (step == 0) {
        step++;
        if (contains(input, len, "loop")) {
            volatile unsigned x = 0;
            for (;;) { x++; }
        }
        if (contains(input, len, "block")) {
            return respond(suspended_resp);
        }
        return respond(completed_resp);
    }
    return respond(blocked_done_resp);
}
