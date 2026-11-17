# Freeze the QuickPy M0 Python modules as source strings.
#
# freeze_as_str avoids mpy-cross for the frozen content itself, but the
# MicroPython build system still requires an mpy-cross binary to exist; the
# build script builds it from the pinned checkout.

freeze_as_str("modules")
