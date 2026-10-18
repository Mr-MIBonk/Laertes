# C:\ProgramData\OE\macros\mcpserver.py
# ODIS Jython TCP bridge for the external MCP server.
# Do NOT put MCP server code in this file.
#
# Protocol: one JSON object per line in each direction; every request has an "action".
# The actions live in mcpserver_handlers.py next to this file. It is reloaded whenever it
# changes, so actions can be added or fixed without restarting this macro.

import os
import socket
import threading
import json
import re
import traceback

from java.lang import Throwable


HOST = "127.0.0.1"
PORT = 8765
MACRO_DIR = r"C:\ProgramData\OE\macros"
HANDLERS_FILE = os.path.join(MACRO_DIR, "mcpserver_handlers.py")

# Shared with the handlers: one diagnostic job at a time on the vehicle.
DIAG_LOCK = threading.Lock()

# Set by the "shutdown" action: the accept loop ends and the macro returns control to ODIS.
_stop = threading.Event()

_loaded = {"mtime": None, "handlers": None, "error": None}
_load_lock = threading.Lock()


def get_handlers():
    """Returns the HANDLERS of mcpserver_handlers.py, reloading the file when it changed.

    If a changed file fails to load, the previous handlers stay active and the error is
    reported with every request until the file is fixed.
    """
    with _load_lock:
        mtime = os.path.getmtime(HANDLERS_FILE)
        if mtime != _loaded["mtime"]:
            _loaded["mtime"] = mtime
            namespace = {"__name__": "mcpserver_handlers", "DIAG_LOCK": DIAG_LOCK}
            try:
                execfile(HANDLERS_FILE, namespace)
                _loaded["handlers"] = namespace["HANDLERS"]
                _loaded["error"] = None
                print("Loaded %s" % HANDLERS_FILE)
            except Exception as e:
                traceback.print_exc()
                _loaded["error"] = "%s: %s" % (e.__class__.__name__, e)
        if _loaded["handlers"] is None:
            raise Exception("cannot load %s: %s" % (HANDLERS_FILE, _loaded["error"]))
        return _loaded["handlers"], _loaded["error"]


def handle_request(cmd):
    action = cmd.get("action")

    if action is None:
        return {
            "ok": False,
            "error": "Missing action"
        }

    if action == "shutdown":
        # Handled here, not in the handlers: the server owns its own lifetime.
        _stop.set()
        return {"ok": True, "stopping": True}

    handlers, load_error = get_handlers()
    handler = handlers.get(action)

    if handler is None:
        response = {
            "ok": False,
            "error": "Unknown action: %s" % action,
            "available_actions": sorted(handlers.keys())
        }
    else:
        response = handler(cmd)

    if load_error:
        response["handlers_error"] = load_error
    return response


def client_loop(conn):
    try:
        buffer = ""

        while True:
            data = conn.recv(65536)

            if not data:
                break

            buffer += data.decode("utf-8")

            while "\n" in buffer:
                line, buffer = buffer.split("\n", 1)

                if not line.strip():
                    continue

                # A web page can send an HTTP request to this port; its request line is not JSON.
                # Drop such connections so a page cannot slip a command in through the body.
                if re.match(r"^[A-Z]+ \S+ HTTP/", line):
                    return

                try:
                    cmd = json.loads(line)
                    response = handle_request(cmd)
                except (Exception, Throwable) as e:
                    traceback.print_exc()
                    # unicode, not str: ODIS messages may contain non-ASCII text.
                    if isinstance(e, Throwable):
                        message = unicode(e.getMessage() or e.toString())
                    else:
                        message = unicode(e)
                    response = {
                        "ok": False,
                        "error": message,
                        "exception_type": e.__class__.__name__
                    }

                try:
                    # default=unicode: a Java object a handler did not convert is sent as text.
                    encoded = (json.dumps(response, default=unicode) + "\n").encode("utf-8")
                    conn.sendall(encoded)
                except Exception as e:
                    traceback.print_exc()
                    return

                if _stop.isSet():
                    # Wake the accept loop so main() returns.
                    try:
                        socket.create_connection((HOST, PORT)).close()
                    except Exception:
                        pass
                    return

    except Exception as e:
        traceback.print_exc()

    finally:
        try:
            conn.close()
        except:
            pass


def main():
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind((HOST, PORT))
    server.listen(5)

    print("ODIS Jython bridge listening on %s:%d" % (HOST, PORT))

    try:
        while not _stop.isSet():
            conn, addr = server.accept()
            if _stop.isSet():
                conn.close()
                break
            print("Client connected:", addr)
            # Daemon threads: a stopped bridge must not keep the macro running.
            t = threading.Thread(target=client_loop, args=(conn,))
            t.setDaemon(True)
            t.start()
    finally:
        server.close()
    print("ODIS bridge stopped; ODIS can be used again.")


main()
