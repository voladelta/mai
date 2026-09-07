"""Mai's private, sequential Python cell runner (standard library only)."""

import ast
import __future__
import json
import os
import sys
import traceback
import types


def main():
    requests = os.fdopen(3, "r", encoding="utf-8")
    responses = os.fdopen(4, "w", encoding="utf-8")
    module = types.ModuleType("__main__")
    sys.modules["__main__"] = module
    namespace = module.__dict__
    namespace["__builtins__"] = __builtins__
    stdout, stderr = sys.stdout, sys.stderr
    future_mask = 0
    for name in __future__.all_feature_names:
        future_mask |= getattr(__future__, name).compiler_flag
    flags = 0

    for line in requests:
        request = json.loads(line)
        ok = True
        try:
            tree = ast.parse(request["code"], filename="<mai>")
            expression = None
            if tree.body and isinstance(tree.body[-1], ast.Expr):
                expression = ast.Expression(tree.body.pop().value)
            program = compile(tree, "<mai>", "exec", flags=flags)
            flags |= program.co_flags & future_mask
            exec(program, namespace)
            if expression is not None:
                value = eval(compile(expression, "<mai>", "eval", flags=flags), namespace)
                if value is not None:
                    print(repr(value))
        except BaseException:
            ok = False
            traceback.print_exc(file=stderr)
        finally:
            # Cells must join their background work before returning. These
            # markers let Go drain both OS streams before returning the result.
            sys.stdout, sys.stderr = stdout, stderr
            stdout.flush()
            stderr.flush()
            marker = request["marker"].encode("ascii")
            os.write(1, marker)
            os.write(2, marker)
        responses.write(json.dumps({"ok": ok}) + "\n")
        responses.flush()


main()
