"""Mai's private Python cell runner and bounded host bridge."""

import ast
import asyncio
import __future__
import collections
import contextvars
import inspect
import json
import linecache
import os
import queue
import signal
import sys
import threading
import traceback
import types

MAX_FRAME = 1 << 20
MAX_PENDING = 8
MAX_CALLS = 64


def main():
    # Fork before starting threads. Only Mai owns the lifetime pipe writer.
    # This process can stop cells that block in native code when Mai dies.
    if os.fork() == 0:
        for fd in range(5):
            os.close(fd)
        try:
            while os.read(5, 1):
                pass
        finally:
            os.killpg(os.getpgrp(), signal.SIGKILL)
        os._exit(0)
    os.close(5)

    requests = os.fdopen(3, "rb")
    responses = os.fdopen(4, "wb")
    generation = int(sys.argv[1])
    cells = queue.Queue(maxsize=1)
    lock = threading.Lock()
    active = None
    last_cell = 0
    pending = {}
    call_id = 0
    effect_calls = 0
    closing = False
    cell_context = contextvars.ContextVar("mai_cell", default=None)
    owner_thread = threading.get_ident()
    loop = asyncio.new_event_loop()

    def send(message):
        data = json.dumps(message, ensure_ascii=False, allow_nan=False).encode("utf-8") + b"\n"
        if len(data) > MAX_FRAME:
            raise ValueError("Python control frame exceeds 1 MiB")
        responses.write(data)
        responses.flush()

    def protocol_error():
        os.write(2, b"Invalid Python host protocol; state was lost.\n")
        os._exit(70)

    def complete_reply(message):
        with lock:
            item = pending.pop(message["call"], None)
        if item is None:
            protocol_error()
        future, _ = item
        if future.done():
            protocol_error()
        future.set_result(message["result"])

    def read_control():
        nonlocal active, last_cell
        try:
            while True:
                line = requests.readline(MAX_FRAME + 1)
                if not line or len(line) > MAX_FRAME or not line.endswith(b"\n"):
                    protocol_error()
                message = json.loads(line)
                if not isinstance(message, dict) or message.get("generation") != generation:
                    protocol_error()
                if type(message.get("cell")) is not int or message["cell"] <= 0:
                    protocol_error()
                if message.get("type") == "execute":
                    if set(message) != {"type", "generation", "cell", "code", "marker"}:
                        protocol_error()
                    if not isinstance(message["code"], str) or not isinstance(message["marker"], str):
                        protocol_error()
                    with lock:
                        if active is not None or message["cell"] <= last_cell:
                            protocol_error()
                        active = last_cell = message["cell"]
                    cells.put_nowait(message)
                elif message.get("type") == "host_reply":
                    if set(message) != {"type", "generation", "cell", "call", "result"}:
                        protocol_error()
                    if type(message["call"]) is not int:
                        protocol_error()
                    with lock:
                        item = pending.get(message["call"])
                        if active != message["cell"] or item is None:
                            protocol_error()
                        _, target_loop = item
                    target_loop.call_soon_threadsafe(complete_reply, message)
                else:
                    protocol_error()
        except BaseException:
            protocol_error()

    async def host_call(name, arguments):
        nonlocal call_id, effect_calls
        if threading.get_ident() != owner_thread:
            raise RuntimeError("Host calls must run on the cell execution thread")
        target_loop = asyncio.get_running_loop()
        future = target_loop.create_future()
        with lock:
            if active is None or closing or cell_context.get() != active:
                raise RuntimeError("Host calls are only available inside an active cell")
            if len(pending) >= MAX_PENDING or name != "child_status" and effect_calls >= MAX_CALLS:
                raise RuntimeError("Python host call limit reached (8 pending, 64 per cell)")
            call_id += 1
            if name != "child_status":
                effect_calls += 1
            identifier = call_id
            pending[identifier] = (future, target_loop)
            cell = active
        try:
            send({"type": "host_call", "generation": generation, "cell": cell,
                  "call": identifier, "name": name, "arguments": arguments})
        except BaseException:
            with lock:
                pending.pop(identifier)
                call_id -= 1
            raise

        # Cancelling an awaiter must not release the operation while Go still
        # owns its effects. Receive its final reply before propagating cancel.
        cancelled = False
        while not future.done():
            try:
                await asyncio.shield(future)
            except asyncio.CancelledError:
                cancelled = True
        if cancelled:
            raise asyncio.CancelledError()
        return future.result()

    mai = types.ModuleType("mai")

    async def bash(command, timeout_ms=0):
        return await host_call("bash", {"command": command, "timeout_ms": timeout_ms})

    async def apply_patch(patch):
        return await host_call("apply_patch", {"patch": patch})

    async def spawn_subagent(name, prompt):
        return await host_call("spawn_subagent", {"name": name, "prompt": prompt})

    class Child:
        def __init__(self, identifier):
            self.id = identifier

        def __repr__(self):
            return "mai.Child(%r)" % self.id

        async def status(self):
            result = await host_call("child_status", {"id": self.id})
            if "error" in result:
                raise RuntimeError(result["error"])
            return result

        async def cancel(self):
            result = await host_call("child_cancel", {"id": self.id})
            if "error" in result:
                raise RuntimeError(result["error"])
            return result

        async def wait(self):
            while True:
                result = await self.status()
                if result["status"] != "running":
                    if result["status"] == "unknown":
                        raise RuntimeError("Child outcome is unknown; do not relaunch automatically")
                    return result["result"]
                await asyncio.sleep(0.1)

    async def spawn(name, prompt):
        result = await host_call("spawn", {"name": name, "prompt": prompt})
        if "error" in result:
            raise RuntimeError(result["error"])
        return Child(result["id"])

    mai.bash, mai.apply_patch, mai.spawn_subagent = bash, apply_patch, spawn_subagent
    mai.spawn = spawn
    sys.modules["mai"] = mai
    module = types.ModuleType("__main__")
    sys.modules["__main__"] = module
    namespace = module.__dict__
    namespace.update({"__builtins__": __builtins__, "mai": mai})
    stdout, stderr = sys.stdout, sys.stderr
    future_mask = 0
    for name in __future__.all_feature_names:
        future_mask |= getattr(__future__, name).compiler_flag
    flags = ast.PyCF_ALLOW_TOP_LEVEL_AWAIT
    sources = collections.OrderedDict()
    source_bytes = 0

    async def finish_tasks():
        while True:
            tasks = asyncio.all_tasks(loop) - {asyncio.current_task(loop)}
            if not tasks:
                return
            for task in tasks:
                task.cancel()
            await asyncio.gather(*tasks, return_exceptions=True)

    async def evaluate_async(program):
        value = eval(program, namespace)
        if program.co_flags & inspect.CO_COROUTINE:
            value = await value
        return value

    async def evaluate_cell(program, expression):
        await evaluate_async(program)
        if expression is not None:
            return await evaluate_async(expression)

    threading.Thread(target=read_control, daemon=True).start()
    send({"type": "ready", "generation": generation, "version": sys.version.split()[0],
          "executable": sys.executable,
          "gil_enabled": getattr(sys, "_is_gil_enabled", lambda: True)()})

    while True:
        request = cells.get()
        cell_context.set(request["cell"])
        with lock:
            call_id = 0
            effect_calls = 0
            closing = False
        filename = "<mai:g%d:c%d>" % (generation, request["cell"])
        code = request["code"]
        size = len(code.encode("utf-8"))
        sources[filename] = size
        source_bytes += size
        linecache.cache[filename] = (len(code), None, code.splitlines(True), filename)
        while len(sources) > 32 or source_bytes > MAX_FRAME:
            old, old_size = sources.popitem(last=False)
            source_bytes -= old_size
            linecache.cache.pop(old, None)
        ok = True
        try:
            tree = ast.parse(code, filename=filename)
            expression = None
            if tree.body and isinstance(tree.body[-1], ast.Expr):
                expression = ast.Expression(tree.body.pop().value)
            program = compile(tree, filename, "exec", flags=flags)
            next_flags = flags | (program.co_flags & future_mask)
            if expression is not None:
                expression = compile(expression, filename, "eval", flags=next_flags)
            flags = next_flags
            if program.co_flags & inspect.CO_COROUTINE or expression is not None and expression.co_flags & inspect.CO_COROUTINE:
                asyncio.set_event_loop(loop)
                value = loop.run_until_complete(evaluate_cell(program, expression))
            else:
                exec(program, namespace)
                value = eval(expression, namespace) if expression is not None else None
            if value is not None:
                print(repr(value))
        except BaseException:
            ok = False
            traceback.print_exc(file=stderr)
        finally:
            with lock:
                closing = True
            asyncio.set_event_loop(loop)
            loop.run_until_complete(finish_tasks())
            sys.stdout, sys.stderr = stdout, stderr
            stdout.flush()
            stderr.flush()
            marker = request["marker"].encode("ascii")
            os.write(1, marker)
            os.write(2, marker)
        with lock:
            active = None
        send({"type": "done", "generation": generation, "cell": request["cell"], "ok": ok})


main()
