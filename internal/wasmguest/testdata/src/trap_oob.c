/* Synthetic host-adapter fixture. Not the QuickPy guest. */
#define EXPORT __attribute__((visibility("default")))
EXPORT int qp_abi_version(void) { return 4; }
EXPORT int qp_alloc(int len) { (void)len; return 1024; }
EXPORT void qp_free(int ptr, int len) { (void)ptr; (void)len; }
EXPORT long long qp_invoke(int ptr, int len) {
    (void)ptr; (void)len;
    /* An arbitrary out-of-bounds store. This is not attributable to a
       configured memory cap and must remain GuestFault. */
    volatile unsigned char *p = (volatile unsigned char *)0x7fffffff;
    *p = 1;
    return 0;
}
