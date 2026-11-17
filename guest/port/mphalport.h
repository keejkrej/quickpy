#ifndef MICROPY_INCLUDED_QUICKPY_MPHALPORT_H
#define MICROPY_INCLUDED_QUICKPY_MPHALPORT_H

#include <stdint.h>

// The M0 spike has no wall clock and no interrupt input.  These are stubs so
// that the core can link; the guest is driven synchronously by qp_invoke.
static inline mp_uint_t mp_hal_ticks_ms(void) {
    return 0;
}

static inline mp_uint_t mp_hal_ticks_us(void) {
    return 0;
}

static inline mp_uint_t mp_hal_ticks_cpu(void) {
    return 0;
}

static inline void mp_hal_set_interrupt_char(int c) {
    (void)c;
}

#endif
