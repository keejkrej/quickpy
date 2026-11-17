# QuickPy M0 asyncio package.
#
# Adapted from MicroPython v1.29.0 extmod/asyncio/__init__.py.
# Stream I/O and ThreadSafeFlag are intentionally unsupported in M0.

from .core import *
from .funcs import wait_for, wait_for_ms, gather
from .event import Event
from .lock import Lock

__version__ = (3, 0, 0)


def open_connection(*args, **kwargs):
    raise UnsupportedError("stream I/O is not supported")


def start_server(*args, **kwargs):
    raise UnsupportedError("stream I/O is not supported")


class StreamReader:
    def __init__(self, *args, **kwargs):
        raise UnsupportedError("stream I/O is not supported")


class StreamWriter:
    def __init__(self, *args, **kwargs):
        raise UnsupportedError("stream I/O is not supported")


class ThreadSafeFlag:
    def __init__(self, *args, **kwargs):
        raise UnsupportedError("ThreadSafeFlag is not supported")
