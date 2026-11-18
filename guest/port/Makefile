# QuickPy M0 guest spike build.
#
# Usage (normally via guest/build.sh):
#   make -C guest/port MPY_DIR=/abs/path/to/micropython BUILD=/abs/path/to/build/obj
#
# The MicroPython checkout is not vendored.  guest/build.sh clones the exact
# pin into guest/.cache/micropython.

PORT_DIR := $(dir $(abspath $(lastword $(MAKEFILE_LIST))))
MPY_DIR ?= $(abspath $(PORT_DIR)../.cache/micropython)
BUILD ?= $(abspath $(PORT_DIR)../build/obj)

include $(MPY_DIR)/py/mkenv.mk

# Must be set before py.mk so it adds frozen_content.o to PY_O.
FROZEN_MANIFEST = $(PORT_DIR)manifest.py

# mkenv.mk sets TOP to the MicroPython checkout.
include $(TOP)/py/py.mk

QSTR_DEFS = qstrdefsport.h
MICROPY_ROM_TEXT_COMPRESSION ?= 0

INC += -I.
INC += -I$(TOP)
INC += -I$(BUILD)

WASM_CC_FLAGS = --target=wasm32-wasi -fwasm-exceptions \
	-Xclang=-target-feature -Xclang=+exception-handling \
	-mllvm=-wasm-enable-sjlj

CC = zig cc
LD = zig cc
CPP = $(PORT_DIR)cpp.sh

CFLAGS += $(INC) -Wall -Wno-error -std=gnu99 -Os -DNDEBUG \
	-fdata-sections -ffunction-sections $(WASM_CC_FLAGS)

LDFLAGS += $(WASM_CC_FLAGS) -mexec-model=reactor \
	-Wl,--gc-sections \
	-Wl,--initial-memory=4194304 \
	-Wl,--max-memory=67108864 \
	-Wl,-z,stack-size=1048576

LIBS =

SRC_C = \
	adapter.c \
	setjmp_rt.c \
	extmod/modasyncio.c \
	extmod/modbinascii.c \
	extmod/modjson.c \
	extmod/modre.c \
	extmod/modtime.c

SRC_QSTR += adapter.c extmod/modasyncio.c extmod/modbinascii.c extmod/modjson.c extmod/modre.c extmod/modtime.c

OBJ += $(PY_O)
OBJ += $(addprefix $(BUILD)/, $(SRC_C:.c=.o))

all: $(BUILD)/quickpy.wasm

$(BUILD)/quickpy.wasm: $(OBJ)
	$(ECHO) "LINK $@"
	$(Q)$(CC) $(LDFLAGS) -o $@ $^ $(LIBS)

# The MicroPython build system normally invokes $(SIZE) after linking; the
# spike does not need it and wasm-opt reports the final size.
SIZE = true
OBJCOPY = true

include $(TOP)/py/mkrules.mk
