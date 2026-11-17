/*
 * WebAssembly setjmp/longjmp runtime for the QuickPy M0 guest spike.
 *
 * Adapted from wasi-libc:
 *   libc-top-half/musl/src/setjmp/wasm32/rt.c
 *   https://github.com/WebAssembly/wasi-libc
 *
 * wasi-libc is MIT licensed.  Zig 0.17.1 ships an older copy of this file that
 * does not define the __c_longjmp tag required by LLVM 22+, so the spike
 * carries the current runtime locally instead of patching the toolchain.
 *
 * This file is compiled with:
 *   -fwasm-exceptions
 *   -Xclang -target-feature -Xclang +exception-handling
 *   -mllvm -wasm-enable-sjlj
 * and the resulting module is translated with:
 *   wasm-opt --translate-to-exnref
 * because wazero v1.12 accepts exnref but rejects the legacy EH opcodes that
 * LLVM still emits for setjmp/longjmp.
 */

#include <stddef.h>
#include <stdint.h>

void __wasm_setjmp(void *env, uint32_t label, void *func_invocation_id);
uint32_t __wasm_setjmp_test(void *env, void *func_invocation_id);
void __wasm_longjmp(void *env, int val);

struct jmp_buf_impl {
    void *func_invocation_id;
    uint32_t label;

    struct arg {
        void *env;
        int val;
    } arg;
};

void
__wasm_setjmp(void *env, uint32_t label, void *func_invocation_id)
{
    struct jmp_buf_impl *jb = env;
    if (label == 0) { /* ABI contract */
        __builtin_trap();
    }
    if (func_invocation_id == NULL) { /* sanity check */
        __builtin_trap();
    }
    jb->func_invocation_id = func_invocation_id;
    jb->label = label;
}

uint32_t
__wasm_setjmp_test(void *env, void *func_invocation_id)
{
    struct jmp_buf_impl *jb = env;
    if (jb->label == 0) { /* ABI contract */
        __builtin_trap();
    }
    if (func_invocation_id == NULL) { /* sanity check */
        __builtin_trap();
    }
    if (jb->func_invocation_id == func_invocation_id) {
        return jb->label;
    }
    return 0;
}

void
__wasm_longjmp(void *env, int val)
{
    struct jmp_buf_impl *jb = env;
    struct arg *arg = &jb->arg;
    /*
     * C standard says:
     * The longjmp function cannot cause the setjmp macro to return
     * the value 0; if val is 0, the setjmp macro returns the value 1.
     */
    if (val == 0) {
        val = 1;
    }
    arg->env = env;
    arg->val = val;
    __builtin_wasm_throw(1, arg); /* 1 == C_LONGJMP */
}

// The `__builtin_wasm_throw` invocation above references this symbol, which
// refers to a WebAssembly tag.  The tag can't be defined in C but can be
// defined with inline assembly.
#if __clang_major__ >= 22
__asm__(".globl __c_longjmp\n"
#if defined(__wasm32__)
        ".tagtype __c_longjmp i32\n"
#elif defined(__wasm64__)
        ".tagtype __c_longjmp i64\n"
#else
#error "Unsupported Wasm architecture"
#endif
        "__c_longjmp:\n");
#endif
