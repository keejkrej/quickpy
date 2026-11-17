# MicroPython asyncio module
# MIT license; Copyright (c) 2019-2020 Damien P. George
#
# Copied from MicroPython v1.29.0 extmod/asyncio/event.py for the QuickPy M0
# spike.  ThreadSafeFlag is omitted because M0 has no host I/O polling.

from . import core


# Event class for primitive events that can be waited on, set, and cleared
class Event:
    def __init__(self):
        self.state = False  # False=unset; True=set
        self.waiting = core.TaskQueue()  # Queue of Tasks waiting on completion of this event

    def is_set(self):
        return self.state

    def set(self):
        # Event becomes set, schedule any tasks waiting on this event
        # Note: This must not be called from anything except the thread running
        # the asyncio loop (i.e. neither hard or soft IRQ, or a different thread).
        while self.waiting.peek():
            core._task_queue.push(self.waiting.pop())
        self.state = True

    def clear(self):
        self.state = False

    # async
    def wait(self):
        if not self.state:
            # Event not set, put the calling task on the event's waiting queue
            self.waiting.push(core.cur_task)
            # Set calling task's data to the event's queue so it can be removed if needed
            core.cur_task.data = self.waiting
            yield
        return True
