// QuickPy M0 guest adapter.
//
// This is an experimental, deliberately small reactor-style WASM/WASI module.
// It embeds MicroPython's compiler + bytecode VM and exposes the transient M0
// ABI described in guest/M0_NOTES.md.
//
// Ownership:
//   - qp_alloc returns a malloc'd buffer owned by the host until qp_free.
//   - qp_invoke does NOT free the request buffer.  The host frees it with
//     qp_free after qp_invoke returns.
//   - qp_invoke returns a malloc'd response buffer.  The host copies the bytes
//     and then calls qp_free(response_ptr, response_len).
//   - A return value of -1 means the adapter could not produce a response
//     (allocation/init failure).  No response buffer is owned by the host.

#include <stdbool.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "py/builtin.h"
#include "py/compile.h"
#include "py/gc.h"
#include "py/lexer.h"
#include "py/mperrno.h"
#include "py/mphal.h"
#include "py/obj.h"
#include "py/objarray.h"
#include "py/objexcept.h"
#include "py/objint.h"
#include "py/objlist.h"
#include "py/objmodule.h"
#include "py/objstr.h"
#include "py/objtuple.h"
#include "py/objtype.h"
#include "py/parse.h"
#include "py/runtime.h"
#include "py/stackctrl.h"

#ifndef MICROPY_HEAP_SIZE
#define MICROPY_HEAP_SIZE (4 * 1024 * 1024)
#endif

// The imported quickpy module owns the main task, pending futures, and the
// asyncio loop globals.  Keep it in a registered GC root, not a bare static
// mp_obj_t.
MP_REGISTER_ROOT_POINTER(mp_obj_t quickpy_module);

// ---------------------------------------------------------------------------
// Growable byte buffer (malloc-backed, outside the MicroPython GC heap).
// ---------------------------------------------------------------------------

typedef struct {
    char *p;
    size_t len;
    size_t cap;
    size_t limit;   // 0 = unlimited
    bool truncated; // hit limit
    bool oom;       // allocation failed
} sb_t;

static int sb_reserve(sb_t *b, size_t extra) {
    if (extra > SIZE_MAX - b->len - 1) {
        return -1;
    }
    size_t need = b->len + extra + 1;
    if (need <= b->cap) {
        return 0;
    }
    size_t cap = b->cap ? b->cap : 256;
    while (cap < need) {
        if (cap > SIZE_MAX / 2) {
            cap = need;
            break;
        }
        cap *= 2;
    }
    char *p = (char *)realloc(b->p, cap);
    if (p == NULL) {
        return -1;
    }
    b->p = p;
    b->cap = cap;
    return 0;
}

static int sb_putn(sb_t *b, const char *s, size_t n) {
    if (b->limit != 0 && b->len + n > b->limit) {
        n = b->limit - b->len;
        // Do not cut a UTF-8 sequence in half; the response must stay valid
        // JSON text.
        while (n > 0 && ((unsigned char)s[n] & 0xc0) == 0x80) {
            n--;
        }
        b->truncated = true;
    }
    if (n == 0) {
        return 0;
    }
    if (sb_reserve(b, n) != 0) {
        b->oom = true;
        return -1;
    }
    memcpy(b->p + b->len, s, n);
    b->len += n;
    b->p[b->len] = '\0';
    return 0;
}

static int sb_puts(sb_t *b, const char *s) {
    return sb_putn(b, s, strlen(s));
}

static int sb_putc(sb_t *b, char c) {
    return sb_putn(b, &c, 1);
}

static void sb_reset(sb_t *b) {
    b->len = 0;
    b->truncated = false;
    b->oom = false;
    if (b->p != NULL) {
        b->p[0] = '\0';
    }
}

static void sb_free(sb_t *b) {
    free(b->p);
    b->p = NULL;
    b->len = 0;
    b->cap = 0;
}

// Append JSON-escaped string content (no surrounding quotes), escaping as
// required by RFC 8259.  Invalid UTF-8 is passed through byte-for-byte; the
// spike assumes text output is valid UTF-8.
static int sb_json_escaped(sb_t *b, const char *s, size_t n) {
    static const char hex[] = "0123456789abcdef";
    for (size_t i = 0; i < n; i++) {
        unsigned char c = (unsigned char)s[i];
        switch (c) {
            case '"': if (sb_puts(b, "\\\"") != 0) return -1; break;
            case '\\': if (sb_puts(b, "\\\\") != 0) return -1; break;
            case '\b': if (sb_puts(b, "\\b") != 0) return -1; break;
            case '\f': if (sb_puts(b, "\\f") != 0) return -1; break;
            case '\n': if (sb_puts(b, "\\n") != 0) return -1; break;
            case '\r': if (sb_puts(b, "\\r") != 0) return -1; break;
            case '\t': if (sb_puts(b, "\\t") != 0) return -1; break;
            default:
                if (c < 0x20) {
                    char esc[6] = {'\\', 'u', '0', '0', hex[c >> 4], hex[c & 0xf]};
                    if (sb_putn(b, esc, sizeof(esc)) != 0) return -1;
                } else {
                    if (sb_putc(b, (char)c) != 0) return -1;
                }
                break;
        }
    }
    return 0;
}

// Append a complete JSON string literal.
static int sb_json_str(sb_t *b, const char *s, size_t n) {
    if (sb_putc(b, '"') != 0) {
        return -1;
    }
    if (sb_json_escaped(b, s, n) != 0) {
        return -1;
    }
    return sb_putc(b, '"');
}

// Append standard base64 (RFC 4648, with padding) for raw bytes.  The input is
// never interpreted as text, so NUL and invalid UTF-8 survive exactly.
static int sb_put_base64(sb_t *b, const unsigned char *data, size_t len) {
    static const char tbl[] =
        "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/";
    size_t i = 0;
    while (i + 3 <= len) {
        uint32_t v = ((uint32_t)data[i] << 16) | ((uint32_t)data[i + 1] << 8) | data[i + 2];
        char out[4] = {
            tbl[(v >> 18) & 0x3f], tbl[(v >> 12) & 0x3f],
            tbl[(v >> 6) & 0x3f], tbl[v & 0x3f],
        };
        if (sb_putn(b, out, sizeof(out)) != 0) return -1;
        i += 3;
    }
    if (len - i == 1) {
        uint32_t v = (uint32_t)data[i] << 16;
        char out[4] = {tbl[(v >> 18) & 0x3f], tbl[(v >> 12) & 0x3f], '=', '='};
        if (sb_putn(b, out, sizeof(out)) != 0) return -1;
    } else if (len - i == 2) {
        uint32_t v = ((uint32_t)data[i] << 16) | ((uint32_t)data[i + 1] << 8);
        char out[4] = {tbl[(v >> 18) & 0x3f], tbl[(v >> 12) & 0x3f], tbl[(v >> 6) & 0x3f], '='};
        if (sb_putn(b, out, sizeof(out)) != 0) return -1;
    }
    return 0;
}

// ---------------------------------------------------------------------------
// Ordered output events.
//
// Every guest output is recorded as one event in emission order.  Text from
// print()/stdout coalesces with an adjacent text event; a console or image
// event always starts a new record, so text is implicitly flushed before the
// next kind.  The legacy "output" string is rebuilt from text events only.
//
// Caps are guest-side safety limits.  The host must still enforce its exact
// MaxOutputBytes/MaxMessageBytes; see guest/M1_NOTES.md.
// ---------------------------------------------------------------------------

#define QP_MAX_TEXT_BYTES (1024 * 1024)
#define QP_MAX_IMAGE_BYTES (1024 * 1024)
#define QP_MAX_IMAGE_TOTAL_BYTES (1024 * 1024)
#define QP_MAX_OUTPUT_EVENTS 1024

enum {
    EV_TEXT = 0,
    EV_CONSOLE = 1,
    EV_IMAGE = 2,
};

typedef struct {
    uint8_t kind;
    uint8_t level;   // console level index
    char *text;      // text/console text, or image MIME
    size_t text_len;
    char *data;      // image raw bytes
    size_t data_len;
} out_event_t;

static out_event_t *g_events;
static size_t g_events_len;
static size_t g_events_cap;
static size_t g_text_bytes;
static size_t g_image_bytes;
static bool g_out_truncated;
static bool g_out_oom;

static int grow_events(size_t need) {
    if (need <= g_events_cap) {
        return 0;
    }
    size_t cap = g_events_cap ? g_events_cap * 2 : 8;
    while (cap < need) {
        cap *= 2;
    }
    out_event_t *p = (out_event_t *)realloc(g_events, cap * sizeof(out_event_t));
    if (p == NULL) {
        return -1;
    }
    g_events = p;
    g_events_cap = cap;
    return 0;
}

static void reset_outputs(void) {
    for (size_t i = 0; i < g_events_len; i++) {
        free(g_events[i].text);
        free(g_events[i].data);
    }
    g_events_len = 0;
    g_text_bytes = 0;
    g_image_bytes = 0;
    g_out_truncated = false;
    g_out_oom = false;
}

// Clamp a text chunk to the remaining text budget without cutting a UTF-8
// sequence in half.  Returns the accepted length and sets *truncated.
static size_t clamp_text(const char *s, size_t n, bool *truncated) {
    if (g_text_bytes >= QP_MAX_TEXT_BYTES) {
        *truncated = true;
        return 0;
    }
    size_t room = QP_MAX_TEXT_BYTES - g_text_bytes;
    if (n > room) {
        n = room;
        while (n > 0 && ((unsigned char)s[n] & 0xc0) == 0x80) {
            n--;
        }
        *truncated = true;
    }
    return n;
}

static int append_text(const char *s, size_t n) {
    if (n == 0) {
        return 0;
    }
    bool truncated = false;
    n = clamp_text(s, n, &truncated);
    if (truncated) {
        g_out_truncated = true;
    }
    if (n == 0) {
        return 0;
    }
    if (g_events_len > 0 && g_events[g_events_len - 1].kind == EV_TEXT) {
        out_event_t *e = &g_events[g_events_len - 1];
        char *p = (char *)realloc(e->text, e->text_len + n + 1);
        if (p == NULL) {
            g_out_oom = true;
            return -1;
        }
        memcpy(p + e->text_len, s, n);
        e->text = p;
        e->text_len += n;
        p[e->text_len] = '\0';
    } else {
        if (g_events_len >= QP_MAX_OUTPUT_EVENTS) {
            g_out_truncated = true;
            return 0;
        }
        if (grow_events(g_events_len + 1) != 0) {
            g_out_oom = true;
            return -1;
        }
        out_event_t *e = &g_events[g_events_len];
        memset(e, 0, sizeof(*e));
        e->kind = EV_TEXT;
        e->text = (char *)malloc(n + 1);
        if (e->text == NULL) {
            g_out_oom = true;
            return -1;
        }
        memcpy(e->text, s, n);
        e->text[n] = '\0';
        e->text_len = n;
        g_events_len++;
    }
    g_text_bytes += n;
    return 0;
}

static int append_console(int level, const char *s, size_t n) {
    if (g_events_len >= QP_MAX_OUTPUT_EVENTS) {
        g_out_truncated = true;
        return 0;
    }
    bool truncated = false;
    n = clamp_text(s, n, &truncated);
    if (truncated) {
        g_out_truncated = true;
    }
    if (grow_events(g_events_len + 1) != 0) {
        g_out_oom = true;
        return -1;
    }
    out_event_t *e = &g_events[g_events_len];
    memset(e, 0, sizeof(*e));
    e->kind = EV_CONSOLE;
    e->level = (uint8_t)level;
    e->text = (char *)malloc(n + 1);
    if (e->text == NULL) {
        g_out_oom = true;
        return -1;
    }
    if (n != 0) {
        memcpy(e->text, s, n);
    }
    e->text[n] = '\0';
    e->text_len = n;
    g_events_len++;
    g_text_bytes += n;
    return 0;
}

// Returns 0 on success, -1 when the event cap is reached, -2 on allocation
// failure.  Size caps are validated by the caller before this is reached.
static int append_image(const char *mime, size_t mime_len, const char *data, size_t data_len) {
    if (g_events_len >= QP_MAX_OUTPUT_EVENTS) {
        return -1;
    }
    if (grow_events(g_events_len + 1) != 0) {
        g_out_oom = true;
        return -2;
    }
    out_event_t *e = &g_events[g_events_len];
    memset(e, 0, sizeof(*e));
    e->kind = EV_IMAGE;
    e->text = (char *)malloc(mime_len + 1);
    e->data = (char *)malloc(data_len ? data_len : 1);
    if (e->text == NULL || e->data == NULL) {
        free(e->text);
        free(e->data);
        e->text = NULL;
        e->data = NULL;
        g_out_oom = true;
        return -2;
    }
    memcpy(e->text, mime, mime_len);
    e->text[mime_len] = '\0';
    e->text_len = mime_len;
    if (data_len != 0) {
        memcpy(e->data, data, data_len);
    }
    e->data_len = data_len;
    g_events_len++;
    g_image_bytes += data_len;
    return 0;
}

static const char *console_level_name(uint8_t level) {
    switch (level) {
        case 0: return "debug";
        case 1: return "info";
        case 2: return "warn";
        case 3: return "error";
        default: return "info";
    }
}

static int parse_console_level(const char *s, size_t n) {
    if (n == 5 && memcmp(s, "debug", 5) == 0) return 0;
    if (n == 4 && memcmp(s, "info", 4) == 0) return 1;
    if (n == 4 && memcmp(s, "warn", 4) == 0) return 2;
    if (n == 5 && memcmp(s, "error", 5) == 0) return 3;
    return -1;
}

// RFC 7230 token character.
static bool is_token_char(unsigned char c) {
    if ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')) {
        return true;
    }
    switch (c) {
        case '!': case '#': case '$': case '%': case '&': case '\'':
        case '*': case '+': case '-': case '.': case '^': case '_':
        case '`': case '|': case '~':
            return true;
        default:
            return false;
    }
}

// A MIME type is exactly "token/token": non-empty, printable, no whitespace or
// control characters, and no parameters.  This keeps the value safe to embed
// in JSON and small enough to bound.
static bool valid_mime(const char *s, size_t n) {
    if (n == 0 || n > 128) {
        return false;
    }
    size_t slashes = 0;
    for (size_t i = 0; i < n; i++) {
        unsigned char c = (unsigned char)s[i];
        if (c == '/') {
            slashes++;
            continue;
        }
        if (!is_token_char(c)) {
            return false;
        }
    }
    if (slashes != 1) {
        return false;
    }
    return s[0] != '/' && s[n - 1] != '/';
}

// ---------------------------------------------------------------------------
// Minimal JSON request parser.
// ---------------------------------------------------------------------------

typedef struct {
    const char *p;
    const char *end;
} jp_t;

typedef struct {
    int64_t v;
    char *type;
    size_t type_len;
    char *source;
    size_t source_len;
    char *filename;
    size_t filename_len;
    char *responses;
    size_t responses_len;
    char *initial_state;
    size_t initial_state_len;
    bool has_initial_state;
    int64_t max_tool_calls;
    bool has_max_tool_calls;
    bool bad_max_tool_calls;
    bool gc;
} request_t;

static void jp_ws(jp_t *j) {
    while (j->p < j->end) {
        char c = *j->p;
        if (c == ' ' || c == '\t' || c == '\n' || c == '\r') {
            j->p++;
        } else {
            break;
        }
    }
}

static int jp_hex(char c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static int jp_utf8(sb_t *b, uint32_t cp) {
    char out[4];
    size_t n;
    if (cp <= 0x7f) {
        out[0] = (char)cp;
        n = 1;
    } else if (cp <= 0x7ff) {
        out[0] = (char)(0xc0 | (cp >> 6));
        out[1] = (char)(0x80 | (cp & 0x3f));
        n = 2;
    } else if (cp <= 0xffff) {
        out[0] = (char)(0xe0 | (cp >> 12));
        out[1] = (char)(0x80 | ((cp >> 6) & 0x3f));
        out[2] = (char)(0x80 | (cp & 0x3f));
        n = 3;
    } else if (cp <= 0x10ffff) {
        out[0] = (char)(0xf0 | (cp >> 18));
        out[1] = (char)(0x80 | ((cp >> 12) & 0x3f));
        out[2] = (char)(0x80 | ((cp >> 6) & 0x3f));
        out[3] = (char)(0x80 | (cp & 0x3f));
        n = 4;
    } else {
        return -1;
    }
    return sb_putn(b, out, n);
}

static int jp_string(jp_t *j, char **out, size_t *out_len) {
    if (j->p >= j->end || *j->p != '"') {
        return -1;
    }
    j->p++;
    sb_t b = {0};
    while (j->p < j->end) {
        unsigned char c = (unsigned char)*j->p++;
        if (c == '"') {
            if (b.p == NULL) {
                b.p = (char *)malloc(1);
                if (b.p == NULL) return -1;
                b.p[0] = '\0';
            }
            *out = b.p;
            *out_len = b.len;
            return 0;
        }
        if (c == '\\') {
            if (j->p >= j->end) break;
            char e = *j->p++;
            switch (e) {
                case '"': if (sb_putc(&b, '"') != 0) goto fail; break;
                case '\\': if (sb_putc(&b, '\\') != 0) goto fail; break;
                case '/': if (sb_putc(&b, '/') != 0) goto fail; break;
                case 'b': if (sb_putc(&b, '\b') != 0) goto fail; break;
                case 'f': if (sb_putc(&b, '\f') != 0) goto fail; break;
                case 'n': if (sb_putc(&b, '\n') != 0) goto fail; break;
                case 'r': if (sb_putc(&b, '\r') != 0) goto fail; break;
                case 't': if (sb_putc(&b, '\t') != 0) goto fail; break;
                case 'u': {
                    if (j->end - j->p < 4) goto fail;
                    uint32_t cp = 0;
                    for (int k = 0; k < 4; k++) {
                        int h = jp_hex(j->p[k]);
                        if (h < 0) goto fail;
                        cp = (cp << 4) | (uint32_t)h;
                    }
                    j->p += 4;
                    // Combine a UTF-16 surrogate pair if present.
                    if (cp >= 0xd800 && cp <= 0xdbff && j->end - j->p >= 6 &&
                        j->p[0] == '\\' && j->p[1] == 'u') {
                        uint32_t lo = 0;
                        bool ok = true;
                        for (int k = 0; k < 4; k++) {
                            int h = jp_hex(j->p[2 + k]);
                            if (h < 0) { ok = false; break; }
                            lo = (lo << 4) | (uint32_t)h;
                        }
                        if (ok && lo >= 0xdc00 && lo <= 0xdfff) {
                            cp = 0x10000 + ((cp - 0xd800) << 10) + (lo - 0xdc00);
                            j->p += 6;
                        }
                    }
                    if (jp_utf8(&b, cp) != 0) goto fail;
                    break;
                }
                default: goto fail;
            }
        } else {
            if (sb_putc(&b, (char)c) != 0) goto fail;
        }
    }
fail:
    sb_free(&b);
    return -1;
}

static int jp_int(jp_t *j, int64_t *out) {
    bool neg = false;
    if (j->p < j->end && *j->p == '-') {
        neg = true;
        j->p++;
    }
    if (j->p >= j->end || *j->p < '0' || *j->p > '9') {
        return -1;
    }
    int64_t v = 0;
    while (j->p < j->end && *j->p >= '0' && *j->p <= '9') {
        int d = *j->p - '0';
        // Reject values that would overflow int64 rather than invoking
        // undefined signed overflow on hostile input.
        if (v > (INT64_MAX - d) / 10) {
            return -1;
        }
        v = v * 10 + d;
        j->p++;
    }
    *out = neg ? -v : v;
    return 0;
}

static int jp_skip_value(jp_t *j);

static int jp_skip_number(jp_t *j) {
    if (j->p < j->end && *j->p == '-') {
        j->p++;
    }
    if (j->p >= j->end || *j->p < '0' || *j->p > '9') {
        return -1;
    }
    while (j->p < j->end && *j->p >= '0' && *j->p <= '9') {
        j->p++;
    }
    if (j->p < j->end && *j->p == '.') {
        j->p++;
        if (j->p >= j->end || *j->p < '0' || *j->p > '9') {
            return -1;
        }
        while (j->p < j->end && *j->p >= '0' && *j->p <= '9') {
            j->p++;
        }
    }
    if (j->p < j->end && (*j->p == 'e' || *j->p == 'E')) {
        j->p++;
        if (j->p < j->end && (*j->p == '+' || *j->p == '-')) {
            j->p++;
        }
        if (j->p >= j->end || *j->p < '0' || *j->p > '9') {
            return -1;
        }
        while (j->p < j->end && *j->p >= '0' && *j->p <= '9') {
            j->p++;
        }
    }
    return 0;
}

static int jp_skip_array(jp_t *j) {
    j->p++; // [
    jp_ws(j);
    if (j->p < j->end && *j->p == ']') { j->p++; return 0; }
    for (;;) {
        if (jp_skip_value(j) != 0) return -1;
        jp_ws(j);
        if (j->p >= j->end) return -1;
        if (*j->p == ',') { j->p++; continue; }
        if (*j->p == ']') { j->p++; return 0; }
        return -1;
    }
}

static int jp_skip_object(jp_t *j) {
    j->p++; // {
    jp_ws(j);
    if (j->p < j->end && *j->p == '}') { j->p++; return 0; }
    for (;;) {
        char *key = NULL;
        size_t key_len = 0;
        if (jp_string(j, &key, &key_len) != 0) return -1;
        free(key);
        jp_ws(j);
        if (j->p >= j->end || *j->p != ':') return -1;
        j->p++;
        if (jp_skip_value(j) != 0) return -1;
        jp_ws(j);
        if (j->p >= j->end) return -1;
        if (*j->p == ',') { j->p++; continue; }
        if (*j->p == '}') { j->p++; return 0; }
        return -1;
    }
}

static int jp_skip_value(jp_t *j) {
    jp_ws(j);
    if (j->p >= j->end) return -1;
    char c = *j->p;
    if (c == '"') {
        char *s = NULL;
        size_t n = 0;
        if (jp_string(j, &s, &n) != 0) return -1;
        free(s);
        return 0;
    }
    if (c == '{') return jp_skip_object(j);
    if (c == '[') return jp_skip_array(j);
    if (c == '-' || (c >= '0' && c <= '9')) {
        return jp_skip_number(j);
    }
    if (j->end - j->p >= 4 && memcmp(j->p, "true", 4) == 0) { j->p += 4; return 0; }
    if (j->end - j->p >= 5 && memcmp(j->p, "false", 5) == 0) { j->p += 5; return 0; }
    if (j->end - j->p >= 4 && memcmp(j->p, "null", 4) == 0) { j->p += 4; return 0; }
    return -1;
}

// Copy one raw JSON value (object, array, string, number, bool, null) into a
// malloc'd, NUL-terminated buffer.  Used to pass tool values/errors through to
// Python's json.loads without re-encoding them in C.
static int jp_value_raw(jp_t *j, char **out, size_t *out_len) {
    jp_ws(j);
    const char *start = j->p;
    if (jp_skip_value(j) != 0) {
        return -1;
    }
    size_t n = (size_t)(j->p - start);
    char *buf = (char *)malloc(n + 1);
    if (buf == NULL) {
        return -1;
    }
    memcpy(buf, start, n);
    buf[n] = '\0';
    *out = buf;
    *out_len = n;
    return 0;
}

static void request_free(request_t *r) {
    free(r->type);
    free(r->source);
    free(r->filename);
    free(r->responses);
    free(r->initial_state);
    memset(r, 0, sizeof(*r));
}

static int parse_request(const char *buf, size_t len, request_t *r) {
    memset(r, 0, sizeof(*r));
    jp_t j = {buf, buf + len};
    jp_ws(&j);
    if (j.p >= j.end || *j.p != '{') return -1;
    j.p++;
    jp_ws(&j);
    if (j.p < j.end && *j.p == '}') return -1; // empty object: missing fields
    for (;;) {
        jp_ws(&j);
        char *key = NULL;
        size_t key_len = 0;
        if (jp_string(&j, &key, &key_len) != 0) return -1;
        jp_ws(&j);
        if (j.p >= j.end || *j.p != ':') { free(key); return -1; }
        j.p++;
        jp_ws(&j);
        int rc = 0;
        if (key_len == 1 && key[0] == 'v') {
            rc = jp_int(&j, &r->v);
        } else if (key_len == 4 && memcmp(key, "type", 4) == 0) {
            free(r->type);
            rc = jp_string(&j, &r->type, &r->type_len);
        } else if (key_len == 6 && memcmp(key, "source", 6) == 0) {
            free(r->source);
            rc = jp_string(&j, &r->source, &r->source_len);
        } else if (key_len == 8 && memcmp(key, "filename", 8) == 0) {
            free(r->filename);
            rc = jp_string(&j, &r->filename, &r->filename_len);
        } else if (key_len == 9 && memcmp(key, "responses", 9) == 0) {
            free(r->responses);
            r->responses = NULL;
            rc = jp_value_raw(&j, &r->responses, &r->responses_len);
        } else if (key_len == 13 && memcmp(key, "initial_state", 13) == 0) {
            free(r->initial_state);
            r->initial_state = NULL;
            r->has_initial_state = false;
            rc = jp_value_raw(&j, &r->initial_state, &r->initial_state_len);
            if (rc == 0) {
                r->has_initial_state = true;
            }
        } else if (key_len == 14 && memcmp(key, "max_tool_calls", 14) == 0) {
            // Consume the whole value first, then require it to be a bare
            // integer.  This keeps a malformed value from aborting the whole
            // request parse with a generic error.
            jp_ws(&j);
            const char *vstart = j.p;
            if (jp_skip_value(&j) != 0) {
                rc = -1;
            } else {
                jp_t sub = {vstart, j.p};
                int64_t v = 0;
                if (jp_int(&sub, &v) == 0) {
                    jp_ws(&sub);
                    if (sub.p == sub.end) {
                        r->max_tool_calls = v;
                        r->has_max_tool_calls = true;
                    } else {
                        r->bad_max_tool_calls = true;
                    }
                } else {
                    r->bad_max_tool_calls = true;
                }
            }
        } else if (key_len == 2 && memcmp(key, "gc", 2) == 0) {
            if (j.end - j.p >= 4 && memcmp(j.p, "true", 4) == 0) {
                j.p += 4;
                r->gc = true;
            } else if (j.end - j.p >= 5 && memcmp(j.p, "false", 5) == 0) {
                j.p += 5;
                r->gc = false;
            } else {
                rc = -1;
            }
        } else {
            rc = jp_skip_value(&j);
        }
        free(key);
        if (rc != 0) {
            request_free(r);
            return -1;
        }
        jp_ws(&j);
        if (j.p >= j.end) { request_free(r); return -1; }
        if (*j.p == ',') { j.p++; continue; }
        if (*j.p == '}') { j.p++; break; }
        request_free(r);
        return -1;
    }
    jp_ws(&j);
    if (j.p != j.end) { request_free(r); return -1; }
    if (r->type == NULL) {
        request_free(r);
        return -1;
    }
    if (r->type_len == 5 && memcmp(r->type, "start", 5) == 0 && r->source == NULL) {
        request_free(r);
        return -1;
    }
    if (r->type_len == 14 && memcmp(r->type, "tool_responses", 14) == 0 && r->responses == NULL) {
        request_free(r);
        return -1;
    }
    if (r->filename == NULL) {
        r->filename = (char *)malloc(1);
        if (r->filename == NULL) { request_free(r); return -1; }
        r->filename[0] = '\0';
        r->filename_len = 0;
    }
    return 0;
}

// ---------------------------------------------------------------------------
// Host-call ledger and the _quickpy C module.
//
// g_calls holds every undelivered call for the lifetime of the instance.
// g_new_calls holds only calls registered during the current qp_invoke, so the
// response can report NEW REQUEST DELTAS separately from ALL UNDELIVERED IDS.
// ---------------------------------------------------------------------------

typedef struct {
    int64_t id;
    char *name;
    char *args;
    size_t args_len;
} call_t;

static call_t **g_calls;
static size_t g_calls_len;
static size_t g_calls_cap;
static call_t **g_new_calls;
static size_t g_new_calls_len;
static size_t g_new_calls_cap;

static int grow_ptr_array(call_t ***arr, size_t *cap, size_t need) {
    if (need <= *cap) {
        return 0;
    }
    size_t new_cap = *cap ? *cap * 2 : 8;
    while (new_cap < need) {
        new_cap *= 2;
    }
    call_t **p = (call_t **)realloc(*arr, new_cap * sizeof(call_t *));
    if (p == NULL) {
        return -1;
    }
    *arr = p;
    *cap = new_cap;
    return 0;
}

static char *dup_cstr(const char *s) {
    size_t n = strlen(s);
    char *p = (char *)malloc(n + 1);
    if (p != NULL) {
        memcpy(p, s, n + 1);
    }
    return p;
}

static call_t *find_call(int64_t id) {
    for (size_t i = 0; i < g_calls_len; i++) {
        if (g_calls[i]->id == id) {
            return g_calls[i];
        }
    }
    return NULL;
}

static int add_call(int64_t id, const char *name, const char *args, size_t args_len) {
    if (find_call(id) != NULL) {
        return -1;
    }
    call_t *c = (call_t *)calloc(1, sizeof(call_t));
    if (c == NULL) {
        return -1;
    }
    c->id = id;
    c->name = dup_cstr(name);
    c->args = (char *)malloc(args_len + 1);
    if (c->name == NULL || c->args == NULL) {
        free(c->name);
        free(c->args);
        free(c);
        return -1;
    }
    memcpy(c->args, args, args_len);
    c->args[args_len] = '\0';
    c->args_len = args_len;

    if (grow_ptr_array(&g_calls, &g_calls_cap, g_calls_len + 1) != 0 ||
        grow_ptr_array(&g_new_calls, &g_new_calls_cap, g_new_calls_len + 1) != 0) {
        free(c->name);
        free(c->args);
        free(c);
        return -1;
    }
    g_calls[g_calls_len++] = c;
    g_new_calls[g_new_calls_len++] = c;
    return 0;
}

static void remove_from_new_calls(call_t *c) {
    for (size_t i = 0; i < g_new_calls_len; i++) {
        if (g_new_calls[i] == c) {
            memmove(&g_new_calls[i], &g_new_calls[i + 1],
                (g_new_calls_len - i - 1) * sizeof(call_t *));
            g_new_calls_len--;
            return;
        }
    }
}

static bool remove_call(int64_t id) {
    for (size_t i = 0; i < g_calls_len; i++) {
        if (g_calls[i]->id == id) {
            call_t *c = g_calls[i];
            memmove(&g_calls[i], &g_calls[i + 1],
                (g_calls_len - i - 1) * sizeof(call_t *));
            g_calls_len--;
            remove_from_new_calls(c);
            free(c->name);
            free(c->args);
            free(c);
            return true;
        }
    }
    return false;
}

static void reset_new_calls(void) {
    // The call objects remain owned by g_calls.
    g_new_calls_len = 0;
}

static int sb_put_int(sb_t *b, int64_t v) {
    char buf[32];
    int n = snprintf(buf, sizeof(buf), "%lld", (long long)v);
    if (n < 0) {
        return -1;
    }
    return sb_putn(b, buf, (size_t)n);
}

static int build_calls_json(sb_t *b) {
    if (sb_putc(b, '[') != 0) return -1;
    for (size_t i = 0; i < g_new_calls_len; i++) {
        call_t *c = g_new_calls[i];
        if (i > 0 && sb_putc(b, ',') != 0) return -1;
        if (sb_puts(b, "{\"id\":") != 0) return -1;
        if (sb_put_int(b, c->id) != 0) return -1;
        if (sb_puts(b, ",\"name\":") != 0) return -1;
        if (sb_json_str(b, c->name, strlen(c->name)) != 0) return -1;
        if (sb_puts(b, ",\"args\":") != 0) return -1;
        if (sb_putn(b, c->args, c->args_len) != 0) return -1;
        if (sb_putc(b, '}') != 0) return -1;
    }
    return sb_putc(b, ']');
}

static int build_awaiting_json(sb_t *b) {
    if (sb_putc(b, '[') != 0) return -1;
    for (size_t i = 0; i < g_calls_len; i++) {
        if (i > 0 && sb_putc(b, ',') != 0) return -1;
        if (sb_put_int(b, g_calls[i]->id) != 0) return -1;
    }
    return sb_putc(b, ']');
}

static mp_obj_t mod_quickpy_register_call(mp_obj_t id_in, mp_obj_t name_in, mp_obj_t args_in) {
    int64_t id = mp_obj_get_int(id_in);
    const char *name = mp_obj_str_get_str(name_in);
    size_t args_len = 0;
    const char *args = mp_obj_str_get_data(args_in, &args_len);
    if (add_call(id, name, args, args_len) != 0) {
        mp_raise_msg(&mp_type_MemoryError, MP_ERROR_TEXT("call registration failed"));
    }
    return mp_const_none;
}
static MP_DEFINE_CONST_FUN_OBJ_3(mod_quickpy_register_call_obj, mod_quickpy_register_call);

static mp_obj_t mod_quickpy_has_pending_calls(void) {
    return mp_obj_new_bool(g_calls_len > 0);
}
static MP_DEFINE_CONST_FUN_OBJ_0(mod_quickpy_has_pending_calls_obj, mod_quickpy_has_pending_calls);

// Structured console output.  The Python wrapper joins arguments and passes an
// exact level string; C validates both defensively so a direct _quickpy call
// cannot inject an unknown level or a non-string payload.
static mp_obj_t mod_quickpy_emit_console(mp_obj_t level_in, mp_obj_t text_in) {
    size_t level_len = 0;
    const char *level = mp_obj_str_get_data(level_in, &level_len);
    int lvl = parse_console_level(level, level_len);
    if (lvl < 0) {
        mp_raise_ValueError(MP_ERROR_TEXT("invalid console level"));
    }
    size_t text_len = 0;
    const char *text = mp_obj_str_get_data(text_in, &text_len);
    if (append_console(lvl, text, text_len) != 0) {
        mp_raise_msg(&mp_type_MemoryError, MP_ERROR_TEXT("console output failed"));
    }
    return mp_const_none;
}
static MP_DEFINE_CONST_FUN_OBJ_2(mod_quickpy_emit_console_obj, mod_quickpy_emit_console);

// Structured image output.  Data must be bytes/bytearray and is copied
// immediately, so later mutation of a bytearray cannot corrupt the event.
static mp_obj_t mod_quickpy_emit_image(mp_obj_t mime_in, mp_obj_t data_in) {
    size_t mime_len = 0;
    const char *mime = mp_obj_str_get_data(mime_in, &mime_len);
    if (!valid_mime(mime, mime_len)) {
        mp_raise_ValueError(MP_ERROR_TEXT("invalid image mime type"));
    }
    if (!mp_obj_is_type(data_in, &mp_type_bytes) &&
        !mp_obj_is_type(data_in, &mp_type_bytearray)) {
        mp_raise_TypeError(MP_ERROR_TEXT("image data must be bytes or bytearray"));
    }
    mp_buffer_info_t bufinfo;
    mp_get_buffer_raise(data_in, &bufinfo, MP_BUFFER_READ);
    if (bufinfo.len > QP_MAX_IMAGE_BYTES) {
        mp_raise_ValueError(MP_ERROR_TEXT("image exceeds the guest size limit"));
    }
    if (g_image_bytes + bufinfo.len > QP_MAX_IMAGE_TOTAL_BYTES) {
        mp_raise_ValueError(MP_ERROR_TEXT("image output exceeds the guest total limit"));
    }
    int rc = append_image(mime, mime_len, (const char *)bufinfo.buf, bufinfo.len);
    if (rc == -1) {
        mp_raise_ValueError(MP_ERROR_TEXT("image output exceeds the guest event limit"));
    }
    if (rc == -2) {
        mp_raise_msg(&mp_type_MemoryError, MP_ERROR_TEXT("image output failed"));
    }
    return mp_const_none;
}
static MP_DEFINE_CONST_FUN_OBJ_2(mod_quickpy_emit_image_obj, mod_quickpy_emit_image);

static const mp_rom_map_elem_t quickpy_module_globals_table[] = {
    { MP_ROM_QSTR(MP_QSTR___name__), MP_ROM_QSTR(MP_QSTR__quickpy) },
    { MP_ROM_QSTR(MP_QSTR_register_call), MP_ROM_PTR(&mod_quickpy_register_call_obj) },
    { MP_ROM_QSTR(MP_QSTR_has_pending_calls), MP_ROM_PTR(&mod_quickpy_has_pending_calls_obj) },
    { MP_ROM_QSTR(MP_QSTR_emit_console), MP_ROM_PTR(&mod_quickpy_emit_console_obj) },
    { MP_ROM_QSTR(MP_QSTR_emit_image), MP_ROM_PTR(&mod_quickpy_emit_image_obj) },
};
static MP_DEFINE_CONST_DICT(quickpy_module_globals, quickpy_module_globals_table);

const mp_obj_module_t mp_module_quickpy = {
    .base = { &mp_type_module },
    .globals = (mp_obj_dict_t *)&quickpy_module_globals,
};

MP_REGISTER_MODULE(MP_QSTR__quickpy, mp_module_quickpy);

// ---------------------------------------------------------------------------
// MicroPython state and output capture.
// ---------------------------------------------------------------------------

static bool g_initialized;
static bool g_started;
static char *g_heap;
static char *g_stack_top;
static sb_t g_exc_type;
static sb_t g_exc_msg;
static sb_t g_exc_tb;

// Structured tool cause for an uncaught ToolError.  Populated from the
// exception's own attributes, never by parsing the traceback text.
static sb_t g_tool_cause_code;
static sb_t g_tool_cause_msg;
static sb_t g_tool_cause_tool;
static bool g_has_tool_cause;
static bool g_has_tool_cause_tool;
static int64_t g_tool_cause_call_id;
static bool g_has_tool_cause_call_id;

static void capture_strn(void *data, const char *str, size_t len) {
    sb_putn((sb_t *)data, str, len);
}

static const mp_print_t g_err_print = {&g_exc_tb, capture_strn};

mp_uint_t mp_hal_stdout_tx_strn(const char *str, size_t len) {
    // Plain stdout is always a text event.  It is never parsed for control
    // sentinels, so a print() that looks like a console/image message stays
    // text.
    append_text(str, len);
    return len;
}

// The spike captures raw text.  Do not translate LF to CRLF; the host decides
// how to present output.
void mp_hal_stdout_tx_strn_cooked(const char *str, size_t len) {
    mp_hal_stdout_tx_strn(str, len);
}

void mp_hal_stdout_tx_str(const char *str) {
    mp_hal_stdout_tx_strn(str, strlen(str));
}

// time.sleep()/sleep_ms()/sleep_us() are patched in quickpy.py to raise
// asyncio.UnsupportedError.  These C fallbacks are only reachable if user code
// bypasses that patch; they must not block the host.
void mp_hal_delay_ms(mp_uint_t ms) {
    (void)ms;
    mp_raise_msg(&mp_type_NotImplementedError, MP_ERROR_TEXT("time.sleep is not supported"));
}

void mp_hal_delay_us(mp_uint_t us) {
    (void)us;
    mp_raise_msg(&mp_type_NotImplementedError, MP_ERROR_TEXT("time.sleep_us is not supported"));
}

// MICROPY_PY_IO is enabled only so py/objstringio.c provides the StringIO
// object used by json.loads().  There is no filesystem, so open() must fail
// loudly rather than silently doing nothing.
mp_obj_t mp_builtin_open(size_t n_args, const mp_obj_t *args, mp_map_t *kwargs) {
    (void)n_args;
    (void)args;
    (void)kwargs;
    mp_raise_msg(&mp_type_OSError, MP_ERROR_TEXT("open is not supported"));
}
MP_DEFINE_CONST_FUN_OBJ_KW(mp_builtin_open_obj, 0, mp_builtin_open);

void gc_collect(void) {
    void *dummy = NULL;
    gc_collect_start();
    if (g_stack_top != NULL && (char *)&dummy < g_stack_top) {
        gc_collect_root(&dummy, ((mp_uint_t)g_stack_top - (mp_uint_t)&dummy) / sizeof(mp_uint_t));
    }
    gc_collect_end();
}

// Port hooks required by the core when external import / filesystem support is
// disabled.  They must exist for linking but are never reachable in this spike.
mp_lexer_t *mp_lexer_new_from_file(qstr filename) {
    (void)filename;
    mp_raise_OSError(MP_ENOENT);
}

mp_import_stat_t mp_import_stat(const char *path) {
    (void)path;
    return MP_IMPORT_STAT_NO_EXIST;
}

void nlr_jump_fail(void *val) {
    (void)val;
    __builtin_trap();
}

void MP_NORETURN __fatal_error(const char *msg) {
    (void)msg;
    __builtin_trap();
}

static void capture_exception(mp_obj_t exc);

static int ensure_init(void) {
    if (g_initialized) {
        return 0;
    }
    nlr_buf_t nlr;
    if (nlr_push(&nlr) == 0) {
        g_heap = (char *)malloc(MICROPY_HEAP_SIZE);
        if (g_heap == NULL) {
            nlr_pop();
            return -1;
        }
        // Guest-side safety caps.  The host must still enforce its own limits.
        // Output caps are compile-time constants in the output-event section.
        g_exc_msg.limit = 4096;
        g_exc_tb.limit = 16 * 1024;
        gc_init(g_heap, g_heap + MICROPY_HEAP_SIZE);
        mp_init();
        mp_stack_set_top(g_stack_top);
        mp_stack_set_limit(512 * 1024);
        MP_STATE_VM(quickpy_module) = mp_import_name(
            MP_QSTR_quickpy, mp_const_none, MP_OBJ_NEW_SMALL_INT(0));
        nlr_pop();
        g_initialized = true;
        return 0;
    }
    // Import/init failure is unrecoverable for this instance, but report the
    // Python exception so the host can see why.
    capture_exception((mp_obj_t)nlr.ret_val);
    return -1;
}

// ---------------------------------------------------------------------------
// Evaluation.
// ---------------------------------------------------------------------------

static void reset_tool_cause(void) {
    sb_reset(&g_tool_cause_code);
    sb_reset(&g_tool_cause_msg);
    sb_reset(&g_tool_cause_tool);
    g_has_tool_cause = false;
    g_has_tool_cause_tool = false;
    g_tool_cause_call_id = 0;
    g_has_tool_cause_call_id = false;
}

// If exc is a quickpy.ToolError (or subclass), copy its structured attributes
// into the tool-cause buffers.  Attribute access is guarded by nlr so a
// hostile exception object cannot abort the adapter.
static void capture_tool_cause(mp_obj_t exc) {
    if (MP_STATE_VM(quickpy_module) == MP_OBJ_NULL) {
        return;
    }
    nlr_buf_t nlr;
    if (nlr_push(&nlr) != 0) {
        // Attribute lookup raised; leave the cause unset.
        return;
    }
    mp_obj_t cls = mp_load_attr(MP_STATE_VM(quickpy_module), MP_QSTR_ToolError);
    if (!mp_obj_is_subclass_fast(MP_OBJ_FROM_PTR(mp_obj_get_type(exc)), cls)) {
        nlr_pop();
        return;
    }

    mp_obj_t code = mp_load_attr(exc, MP_QSTR_code);
    size_t code_len = 0;
    const char *code_str = mp_obj_str_get_data(code, &code_len);
    sb_putn(&g_tool_cause_code, code_str, code_len);

    mp_obj_t message = mp_load_attr(exc, MP_QSTR_message);
    size_t msg_len = 0;
    const char *msg_str = mp_obj_str_get_data(message, &msg_len);
    sb_putn(&g_tool_cause_msg, msg_str, msg_len);

    mp_obj_t tool = mp_load_attr(exc, MP_QSTR_tool);
    if (tool != mp_const_none) {
        size_t tool_len = 0;
        const char *tool_str = mp_obj_str_get_data(tool, &tool_len);
        sb_putn(&g_tool_cause_tool, tool_str, tool_len);
        g_has_tool_cause_tool = true;
    }

    mp_obj_t call_id = mp_load_attr(exc, MP_QSTR_call_id);
    if (call_id != mp_const_none) {
        g_tool_cause_call_id = mp_obj_get_int(call_id);
        g_has_tool_cause_call_id = true;
    }

    g_has_tool_cause = true;
    nlr_pop();
}

static void capture_exception(mp_obj_t exc) {
    const char *type_name = "BaseException";
    if (mp_obj_is_exception_instance(exc)) {
        type_name = qstr_str(mp_obj_get_type(exc)->name);
    }
    sb_puts(&g_exc_type, type_name);

    if (mp_obj_is_exception_instance(exc)) {
        mp_obj_t value = mp_obj_exception_get_value(exc);
        if (value != mp_const_none) {
            mp_print_t msg_print = {&g_exc_msg, capture_strn};
            mp_obj_print_helper(&msg_print, value, PRINT_STR);
        }
        capture_tool_cause(exc);
    }

    mp_obj_print_exception(&g_err_print, exc);
}

static int run_source(const char *source, size_t source_len, const char *filename) {
    nlr_buf_t nlr;
    if (nlr_push(&nlr) == 0) {
        qstr filename_qstr = qstr_from_strn(filename, strlen(filename));
        mp_lexer_t *lex = mp_lexer_new_from_str_len(filename_qstr, source, source_len, 0);
        qstr source_name = lex->source_name;
        mp_parse_tree_t parse_tree = mp_parse(lex, MP_PARSE_FILE_INPUT);
        mp_obj_t module_fun = mp_compile(&parse_tree, source_name, true);
        mp_call_function_0(module_fun);
        nlr_pop();
        return 0;
    }

    // Uncaught Python exception.  nlr.ret_val is the exception object.
    capture_exception((mp_obj_t)nlr.ret_val);
    return 1;
}

static bool type_is(const request_t *r, const char *s) {
    size_t n = strlen(s);
    return r->type_len == n && memcmp(r->type, s, n) == 0;
}

static void set_protocol_error(const char *msg) {
    sb_reset(&g_exc_type);
    sb_reset(&g_exc_msg);
    sb_reset(&g_exc_tb);
    reset_tool_cause();
    sb_puts(&g_exc_type, "ProtocolError");
    sb_puts(&g_exc_msg, msg);
}

static int get_quickpy_state(char *buf, size_t buflen) {
    if (MP_STATE_VM(quickpy_module) == MP_OBJ_NULL) {
        snprintf(buf, buflen, "idle");
        return 0;
    }
    mp_obj_t state = mp_load_attr(MP_STATE_VM(quickpy_module), MP_QSTR__state);
    size_t len = 0;
    const char *s = mp_obj_str_get_data(state, &len);
    if (len >= buflen) {
        len = buflen - 1;
    }
    memcpy(buf, s, len);
    buf[len] = '\0';
    return 0;
}

static int call_python_noargs(qstr name) {
    nlr_buf_t nlr;
    if (nlr_push(&nlr) == 0) {
        mp_obj_t fun = mp_load_attr(MP_STATE_VM(quickpy_module), name);
        mp_call_function_0(fun);
        nlr_pop();
        return 0;
    }
    capture_exception((mp_obj_t)nlr.ret_val);
    return 1;
}

// Returns 0 on success, -1 if _deliver returned false, -2 on Python exception.
static int call_python_deliver(int64_t id, bool ok, const char *value_json, const char *error_json) {
    nlr_buf_t nlr;
    if (nlr_push(&nlr) == 0) {
        mp_obj_t fun = mp_load_attr(MP_STATE_VM(quickpy_module), MP_QSTR__deliver);
        mp_obj_t args[4];
        args[0] = mp_obj_new_int((mp_int_t)id);
        args[1] = mp_obj_new_bool(ok);
        args[2] = value_json ? mp_obj_new_str(value_json, strlen(value_json)) : mp_const_none;
        args[3] = error_json ? mp_obj_new_str(error_json, strlen(error_json)) : mp_const_none;
        mp_obj_t res = mp_call_function_n_kw(fun, 4, 0, args);
        int rc = mp_obj_is_true(res) ? 0 : -1;
        nlr_pop();
        return rc;
    }
    capture_exception((mp_obj_t)nlr.ret_val);
    return -2;
}

// Returns 0 on success, 1 on Python exception (captured).
static int call_python_set_initial_state(const char *json, size_t len) {
    nlr_buf_t nlr;
    if (nlr_push(&nlr) == 0) {
        mp_obj_t fun = mp_load_attr(MP_STATE_VM(quickpy_module), MP_QSTR__set_initial_state);
        mp_obj_t arg = json ? mp_obj_new_str(json, len) : mp_const_none;
        mp_call_function_1(fun, arg);
        nlr_pop();
        return 0;
    }
    capture_exception((mp_obj_t)nlr.ret_val);
    return 1;
}

// Returns 0 on success, 1 on Python exception (captured).
static int call_python_set_max_tool_calls(int64_t value) {
    nlr_buf_t nlr;
    if (nlr_push(&nlr) == 0) {
        mp_obj_t fun = mp_load_attr(MP_STATE_VM(quickpy_module), MP_QSTR__set_max_tool_calls);
        mp_call_function_1(fun, mp_obj_new_int_from_ll((long long)value));
        nlr_pop();
        return 0;
    }
    capture_exception((mp_obj_t)nlr.ret_val);
    return 1;
}

// Returns 0 on success, 1 on Python exception (captured), -1 on allocation
// failure.  *out is NULL when no set_state() call occurred; otherwise it is a
// malloc'd JSON text owned by the caller (including the text "null").
static int call_python_state_json(char **out, size_t *out_len) {
    *out = NULL;
    *out_len = 0;
    nlr_buf_t nlr;
    if (nlr_push(&nlr) == 0) {
        mp_obj_t fun = mp_load_attr(MP_STATE_VM(quickpy_module), MP_QSTR__state_json);
        mp_obj_t res = mp_call_function_0(fun);
        if (res != mp_const_none) {
            size_t len = 0;
            const char *s = mp_obj_str_get_data(res, &len);
            char *buf = (char *)malloc(len + 1);
            if (buf == NULL) {
                nlr_pop();
                return -1;
            }
            memcpy(buf, s, len);
            buf[len] = '\0';
            *out = buf;
            *out_len = len;
        }
        nlr_pop();
        return 0;
    }
    capture_exception((mp_obj_t)nlr.ret_val);
    return 1;
}

typedef struct {
    int64_t id;
    bool ok;
    char *value;
    size_t value_len;
    char *error;
    size_t error_len;
} tool_response_t;

static void tool_response_free(tool_response_t *r) {
    free(r->value);
    free(r->error);
    memset(r, 0, sizeof(*r));
}

static int parse_one_response(jp_t *j, tool_response_t *r) {
    memset(r, 0, sizeof(*r));
    r->id = -1;
    bool have_id = false;
    bool have_ok = false;
    jp_ws(j);
    if (j->p >= j->end || *j->p != '{') return -1;
    j->p++;
    jp_ws(j);
    if (j->p < j->end && *j->p == '}') return -1;
    for (;;) {
        jp_ws(j);
        char *key = NULL;
        size_t key_len = 0;
        if (jp_string(j, &key, &key_len) != 0) return -1;
        jp_ws(j);
        if (j->p >= j->end || *j->p != ':') { free(key); return -1; }
        j->p++;
        jp_ws(j);
        int rc = 0;
        if (key_len == 2 && memcmp(key, "id", 2) == 0) {
            rc = jp_int(j, &r->id);
            have_id = true;
        } else if (key_len == 2 && memcmp(key, "ok", 2) == 0) {
            if (j->end - j->p >= 4 && memcmp(j->p, "true", 4) == 0) {
                j->p += 4;
                r->ok = true;
            } else if (j->end - j->p >= 5 && memcmp(j->p, "false", 5) == 0) {
                j->p += 5;
                r->ok = false;
            } else {
                rc = -1;
            }
            have_ok = true;
        } else if (key_len == 5 && memcmp(key, "value", 5) == 0) {
            free(r->value);
            r->value = NULL;
            rc = jp_value_raw(j, &r->value, &r->value_len);
        } else if (key_len == 5 && memcmp(key, "error", 5) == 0) {
            free(r->error);
            r->error = NULL;
            rc = jp_value_raw(j, &r->error, &r->error_len);
        } else {
            rc = jp_skip_value(j);
        }
        free(key);
        if (rc != 0) {
            tool_response_free(r);
            return -1;
        }
        jp_ws(j);
        if (j->p >= j->end) {
            tool_response_free(r);
            return -1;
        }
        if (*j->p == ',') { j->p++; continue; }
        if (*j->p == '}') { j->p++; break; }
        tool_response_free(r);
        return -1;
    }
    if (!have_id || !have_ok) {
        tool_response_free(r);
        return -1;
    }
    return 0;
}

// Returns 0 on success, -1 malformed JSON, -2 unknown/duplicate id,
// -3 Python exception during delivery, -4 _deliver returned false.
static int deliver_responses(const char *json, size_t len) {
    jp_t j = {json, json + len};
    jp_ws(&j);
    if (j.p >= j.end || *j.p != '[') return -1;
    j.p++;
    jp_ws(&j);
    if (j.p < j.end && *j.p == ']') {
        j.p++;
        jp_ws(&j);
        return j.p == j.end ? 0 : -1;
    }
    for (;;) {
        tool_response_t r;
        if (parse_one_response(&j, &r) != 0) return -1;
        if (find_call(r.id) == NULL) {
            tool_response_free(&r);
            return -2;
        }
        int rc = call_python_deliver(r.id, r.ok, r.value, r.error);
        if (rc == -2) {
            tool_response_free(&r);
            return -3;
        }
        if (rc != 0) {
            tool_response_free(&r);
            return -4;
        }
        remove_call(r.id);
        tool_response_free(&r);
        jp_ws(&j);
        if (j.p >= j.end) return -1;
        if (*j.p == ',') { j.p++; continue; }
        if (*j.p == ']') { j.p++; break; }
        return -1;
    }
    jp_ws(&j);
    return j.p == j.end ? 0 : -1;
}

// Append the engine identity fields.  The version comes from the pinned
// MicroPython checkout's MICROPY_VERSION_STRING macro, not a hardcoded date.
static int sb_put_engine(sb_t *b) {
    if (sb_puts(b, ",\"engine_name\":\"micropython\",\"engine_version\":\"") != 0) return -1;
    if (sb_puts(b, MICROPY_VERSION_STRING) != 0) return -1;
    return sb_putc(b, '"');
}

// Legacy "output" is the concatenation of TEXT events only.  Console and image
// events are never folded into it; the host must prefer "outputs" when present
// and must not concatenate the legacy string again.
static int build_legacy_output(sb_t *b) {
    if (sb_putc(b, '"') != 0) return -1;
    for (size_t i = 0; i < g_events_len; i++) {
        if (g_events[i].kind == EV_TEXT) {
            if (sb_json_escaped(b, g_events[i].text ? g_events[i].text : "",
                    g_events[i].text_len) != 0) {
                return -1;
            }
        }
    }
    return sb_putc(b, '"');
}

static int build_outputs_json(sb_t *b) {
    if (sb_putc(b, '[') != 0) return -1;
    for (size_t i = 0; i < g_events_len; i++) {
        out_event_t *e = &g_events[i];
        if (i > 0 && sb_putc(b, ',') != 0) return -1;
        if (e->kind == EV_TEXT) {
            if (sb_puts(b, "{\"type\":\"text\",\"text\":") != 0) return -1;
            if (sb_json_str(b, e->text ? e->text : "", e->text_len) != 0) return -1;
            if (sb_putc(b, '}') != 0) return -1;
        } else if (e->kind == EV_CONSOLE) {
            if (sb_puts(b, "{\"type\":\"console\",\"level\":\"") != 0) return -1;
            if (sb_puts(b, console_level_name(e->level)) != 0) return -1;
            if (sb_puts(b, "\",\"text\":") != 0) return -1;
            if (sb_json_str(b, e->text ? e->text : "", e->text_len) != 0) return -1;
            if (sb_putc(b, '}') != 0) return -1;
        } else {
            if (sb_puts(b, "{\"type\":\"image\",\"mime\":") != 0) return -1;
            if (sb_json_str(b, e->text ? e->text : "", e->text_len) != 0) return -1;
            if (sb_puts(b, ",\"data_base64\":\"") != 0) return -1;
            if (sb_put_base64(b, (const unsigned char *)(e->data ? e->data : ""),
                    e->data_len) != 0) {
                return -1;
            }
            if (sb_putc(b, '"') != 0) return -1;
            if (sb_putc(b, '}') != 0) return -1;
        }
    }
    return sb_putc(b, ']');
}

static int build_response(sb_t *resp, int failed, bool state_written,
    const char *state_json, size_t state_json_len) {
    if (sb_puts(resp, "{\"v\":0,\"type\":") != 0) return -1;
    if (sb_puts(resp, failed ? "\"failed\"" : "\"completed\"") != 0) return -1;
    if (sb_put_engine(resp) != 0) return -1;
    if (sb_puts(resp, ",\"output\":") != 0) return -1;
    if (build_legacy_output(resp) != 0) return -1;
    if (sb_puts(resp, ",\"outputs\":") != 0) return -1;
    if (build_outputs_json(resp) != 0) return -1;
    if (g_out_truncated || g_out_oom) {
        if (sb_puts(resp, ",\"output_truncated\":true") != 0) return -1;
    }
    if (failed) {
        if (sb_puts(resp, ",\"error\":{\"type\":") != 0) return -1;
        if (sb_json_str(resp, g_exc_type.p ? g_exc_type.p : "Exception", g_exc_type.len) != 0) return -1;
        if (sb_puts(resp, ",\"message\":") != 0) return -1;
        if (sb_json_str(resp, g_exc_msg.p ? g_exc_msg.p : "", g_exc_msg.len) != 0) return -1;
        if (sb_puts(resp, ",\"traceback\":") != 0) return -1;
        if (sb_json_str(resp, g_exc_tb.p ? g_exc_tb.p : "", g_exc_tb.len) != 0) return -1;
        if (g_exc_tb.truncated || g_exc_tb.oom) {
            if (sb_puts(resp, ",\"traceback_truncated\":true") != 0) return -1;
        }
        if (g_has_tool_cause) {
            if (sb_puts(resp, ",\"tool_cause\":{\"code\":") != 0) return -1;
            if (sb_json_str(resp, g_tool_cause_code.p ? g_tool_cause_code.p : "", g_tool_cause_code.len) != 0) return -1;
            if (sb_puts(resp, ",\"message\":") != 0) return -1;
            if (sb_json_str(resp, g_tool_cause_msg.p ? g_tool_cause_msg.p : "", g_tool_cause_msg.len) != 0) return -1;
            if (g_has_tool_cause_tool) {
                if (sb_puts(resp, ",\"tool\":") != 0) return -1;
                if (sb_json_str(resp, g_tool_cause_tool.p ? g_tool_cause_tool.p : "", g_tool_cause_tool.len) != 0) return -1;
            }
            if (g_has_tool_cause_call_id) {
                if (sb_puts(resp, ",\"call_id\":") != 0) return -1;
                if (sb_put_int(resp, g_tool_cause_call_id) != 0) return -1;
            }
            if (sb_putc(resp, '}') != 0) return -1;
        }
        if (sb_puts(resp, "}") != 0) return -1;
    } else {
        // State is only offered on a successful completed response.  The
        // field is always present so the host can distinguish "no write"
        // (false, no state) from set_state(None) (true, state:null).
        if (sb_puts(resp, ",\"state_written\":") != 0) return -1;
        if (sb_puts(resp, state_written ? "true" : "false") != 0) return -1;
        if (state_written) {
            if (sb_puts(resp, ",\"state\":") != 0) return -1;
            if (sb_putn(resp, state_json, state_json_len) != 0) return -1;
        }
    }
    return sb_puts(resp, "}");
}

static int build_suspended_response(sb_t *resp, int stalled) {
    if (sb_puts(resp, "{\"v\":0,\"type\":\"suspended\",\"reason\":") != 0) return -1;
    // "stalled" is terminal: unfinished main task, no runnable work, no
    // undelivered calls, no timers.  "runnable" is reserved for a future
    // non-terminal early return and is not emitted by this build.
    if (sb_puts(resp, stalled ? "\"stalled\"" : "\"awaiting_calls\"") != 0) return -1;
    if (sb_put_engine(resp) != 0) return -1;
    if (stalled) {
        if (sb_puts(resp, ",\"stalled\":true") != 0) return -1;
    }
    if (sb_puts(resp, ",\"calls\":") != 0) return -1;
    if (build_calls_json(resp) != 0) return -1;
    if (sb_puts(resp, ",\"awaiting\":") != 0) return -1;
    if (build_awaiting_json(resp) != 0) return -1;
    if (sb_puts(resp, ",\"output\":") != 0) return -1;
    if (build_legacy_output(resp) != 0) return -1;
    if (sb_puts(resp, ",\"outputs\":") != 0) return -1;
    if (build_outputs_json(resp) != 0) return -1;
    if (g_out_truncated || g_out_oom) {
        if (sb_puts(resp, ",\"output_truncated\":true") != 0) return -1;
    }
    return sb_puts(resp, "}");
}

// ---------------------------------------------------------------------------
// Exported ABI.
// ---------------------------------------------------------------------------

__attribute__((export_name("qp_abi_version")))
int32_t qp_abi_version(void) {
    // Packed major/minor 0.4.  ABI4 adds the ordered structured "outputs"
    // array (text/console/image) and the dedicated quickpy.console /
    // quickpy.image guest APIs.  The envelope stays v:0 and every ABI3 field
    // (max_tool_calls, tool causes, engine identity, state) is unchanged.  An
    // ABI3 host would ignore "outputs" and lose console/image output, so the
    // host must require 4 and reject older versions.
    return 4;
}

__attribute__((export_name("qp_alloc")))
int32_t qp_alloc(int32_t len) {
    if (len <= 0) {
        return 0;
    }
    void *p = malloc((size_t)len);
    return (int32_t)(uintptr_t)p;
}

__attribute__((export_name("qp_free")))
void qp_free(int32_t ptr, int32_t len) {
    (void)len;
    free((void *)(uintptr_t)(uint32_t)ptr);
}

__attribute__((export_name("qp_invoke")))
int64_t qp_invoke(int32_t ptr, int32_t len) {
    volatile int stack_dummy = 0;
    g_stack_top = (char *)&stack_dummy;

    if (ptr == 0 || len < 0) {
        return -1;
    }

    // Reset per-invocation capture and the NEW-request delta.  g_calls (all
    // undelivered calls) intentionally persists across invokes.  Output events
    // are a per-response delta: a later resume never replays earlier output.
    reset_outputs();
    sb_reset(&g_exc_type);
    sb_reset(&g_exc_msg);
    sb_reset(&g_exc_tb);
    reset_tool_cause();
    reset_new_calls();

    if (ensure_init() != 0) {
        sb_t resp = {0};
        if (build_response(&resp, 1, false, NULL, 0) != 0) {
            sb_free(&resp);
            return -1;
        }
        uint64_t packed = ((uint64_t)(uint32_t)(uintptr_t)resp.p << 32) | (uint32_t)resp.len;
        return (int64_t)packed;
    }

    request_t req;
    if (parse_request((const char *)(uintptr_t)(uint32_t)ptr, (size_t)len, &req) != 0) {
        sb_t resp = {0};
        if (sb_puts(&resp, "{\"v\":0,\"type\":\"failed\",\"engine_name\":\"micropython\",\"engine_version\":\"") != 0 ||
            sb_puts(&resp, MICROPY_VERSION_STRING) != 0 ||
            sb_puts(&resp, "\",\"output\":\"\",\"outputs\":[],\"error\":{\"type\":\"ProtocolError\",\"message\":\"invalid request JSON\"}}") != 0) {
            sb_free(&resp);
            return -1;
        }
        uint64_t packed = ((uint64_t)(uint32_t)(uintptr_t)resp.p << 32) | (uint32_t)resp.len;
        return (int64_t)packed;
    }

    int failed = 0;
    char state[32] = "idle";

    if (req.v != 0) {
        set_protocol_error("unsupported protocol version");
        failed = 1;
    } else if (req.gc) {
        // Test hook: force a real collection at a safe adapter boundary while
        // tasks/futures may be parked.
        gc_collect();
    }

    if (!failed) {
        if (type_is(&req, "start")) {
            if (g_started) {
                set_protocol_error("start already executed");
                failed = 1;
            } else if (req.bad_max_tool_calls) {
                set_protocol_error("max_tool_calls must be an integer");
                failed = 1;
            } else if (!req.has_max_tool_calls) {
                // ABI3 requires the cap on every start.  Silently defaulting
                // would run without the host's configured limit.
                set_protocol_error("start is missing max_tool_calls");
                failed = 1;
            } else if (req.max_tool_calls < -1) {
                set_protocol_error("max_tool_calls must be -1 (unlimited) or >= 0");
                failed = 1;
            } else {
                if (call_python_set_max_tool_calls(req.max_tool_calls) != 0) {
                    failed = 1;
                }
                // initial_state is optional.  Absent and explicit null both
                // mean Python None; the value is validated and copied by the
                // guest before any source runs.
                if (!failed && req.has_initial_state) {
                    if (call_python_set_initial_state(req.initial_state, req.initial_state_len) != 0) {
                        failed = 1;
                    }
                }
                if (!failed) {
                    g_started = true;
                    failed = run_source(req.source, req.source_len, req.filename);
                }
            }
        } else if (type_is(&req, "tool_responses")) {
            get_quickpy_state(state, sizeof(state));
            if (strcmp(state, "suspended") != 0 && strcmp(state, "stalled") != 0) {
                set_protocol_error("no suspended run");
                failed = 1;
            } else {
                int rc = deliver_responses(req.responses, req.responses_len);
                if (rc == -1) {
                    set_protocol_error("invalid tool_responses");
                    failed = 1;
                } else if (rc == -2 || rc == -4) {
                    set_protocol_error("unknown or duplicate call id");
                    failed = 1;
                } else if (rc == -3) {
                    // Python exception during _deliver; details are captured.
                    failed = 1;
                } else {
                    failed = call_python_noargs(MP_QSTR__resume);
                }
            }
        } else if (type_is(&req, "resume")) {
            get_quickpy_state(state, sizeof(state));
            if (strcmp(state, "suspended") != 0 && strcmp(state, "stalled") != 0) {
                set_protocol_error("no suspended run");
                failed = 1;
            } else {
                failed = call_python_noargs(MP_QSTR__resume);
            }
        } else {
            set_protocol_error("unsupported request type");
            failed = 1;
        }
    }

    bool state_written = false;
    char *state_json = NULL;
    size_t state_json_len = 0;

    if (!failed) {
        get_quickpy_state(state, sizeof(state));
        // Only a genuinely completed execution may offer committable state.
        // Suspended/stalled responses keep the pending value guest-side.
        if (strcmp(state, "completed") == 0 || strcmp(state, "idle") == 0) {
            int src = call_python_state_json(&state_json, &state_json_len);
            if (src == 1) {
                failed = 1;
            } else if (src == -1) {
                set_protocol_error("state serialization failed");
                failed = 1;
            } else {
                state_written = (state_json != NULL);
            }
        }
    }

    sb_t resp = {0};
    int rc;
    if (failed) {
        rc = build_response(&resp, 1, false, NULL, 0);
    } else if (strcmp(state, "suspended") == 0) {
        rc = build_suspended_response(&resp, 0);
    } else if (strcmp(state, "stalled") == 0) {
        rc = build_suspended_response(&resp, 1);
    } else if (strcmp(state, "completed") == 0 || strcmp(state, "idle") == 0) {
        rc = build_response(&resp, 0, state_written, state_json, state_json_len);
    } else {
        set_protocol_error("adapter state not quiescent");
        rc = build_response(&resp, 1, false, NULL, 0);
    }
    free(state_json);
    if (rc != 0) {
        sb_free(&resp);
        request_free(&req);
        return -1;
    }

    request_free(&req);

    uint64_t packed = ((uint64_t)(uint32_t)(uintptr_t)resp.p << 32) | (uint32_t)resp.len;
    return (int64_t)packed;
}
