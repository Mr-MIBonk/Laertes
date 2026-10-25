# C:\ProgramData\OE\macros\mcpserver.py
# -*- coding: utf-8 -*-
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
# The development connection (devbridge.py): one client at a time, on its own port. While it is
# connected, commands on PORT (Laertes) are refused, so the two never drive ODIS at once.
DEV_PORT = 8766
MACRO_DIR = r"C:\ProgramData\OE\macros"
HANDLERS_FILE = os.path.join(MACRO_DIR, "mcpserver_handlers.py")

# Shared with the handlers: one diagnostic job at a time on the vehicle.
DIAG_LOCK = threading.Lock()

# Set by the "shutdown" action: the accept loop ends and the macro returns control to ODIS.
_stop = threading.Event()

_loaded = {"mtime": None, "handlers": None, "error": None}
# State that must outlive a handler reload (e.g. background jobs in flight), injected into the
# handlers namespace so a reload does not orphan a job the web page is still polling.
_persist = {}
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
            namespace = {"__name__": "mcpserver_handlers", "DIAG_LOCK": DIAG_LOCK, "_PERSIST": _persist}
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


def client_loop(conn, dev=False):
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
                    if not dev and _dev_slot.locked():
                        response = {"ok": False, "error": DEV_ACTIVE}
                    else:
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


# One client at a time: two clients driving ODIS at once would interfere on the vehicle. Laertes
# keeps one connection open for all its requests; the slot is free again when it disconnects.
_client_slot = threading.Lock()
SINGLE_ACCESS = ("single access limit: another client is already connected to the ODIS bridge; "
                 "close it (e.g. the other Laertes) and try again")


_dev_slot = threading.Lock()
DEV_ACTIVE = ("development connection active: the ODIS bridge serves only it until it disconnects "
              "(close devbridge.py)")
DEV_BUSY = ("single access limit: another development connection is already open (if its command "
            "hangs in ODIS, a shutdown command still stops the bridge; Laertes starts it again)")


def serve_client(conn, slot=_client_slot, dev=False):
    try:
        client_loop(conn, dev)
    finally:
        slot.release()
        print("Development client disconnected; Laertes can use the bridge again" if dev else "Client disconnected")


def reject_client(conn, error=SINGLE_ACCESS, allow_shutdown=False):
    """Tells a client that arrives while another is connected about the limit, as the reply to
    its first command (read first: closing on unread data resets the connection and can lose the
    reply), and closes its connection. In its own thread, so the accept loop is not held up.
    allow_shutdown: a "shutdown" command is still obeyed (devbridge.py '{"action": "shutdown"}'), the way out
    when a development command hangs inside ODIS and so keeps its slot, and Laertes, blocked."""
    print("Client rejected: another client is connected")

    def reply():
        try:
            conn.settimeout(5)
            data = ""
            while "\n" not in data:
                chunk = conn.recv(65536)
                if not chunk:
                    break
                data += chunk
            if allow_shutdown and data.strip() and json.loads(data.split("\n")[0]).get("action") == "shutdown":
                _stop.set()
                conn.sendall((json.dumps({"ok": True, "stopping": True}) + "\n").encode("utf-8"))
                socket.create_connection((HOST, PORT)).close()  # wake the accept loop so main() returns
                return
            conn.sendall((json.dumps({"ok": False, "error": error}) + "\n").encode("utf-8"))
        except Exception:
            pass
        finally:
            try:
                conn.close()
            except Exception:
                pass

    t = threading.Thread(target=reply)
    t.setDaemon(True)
    t.start()


def listen(port):
    server = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    server.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    server.bind((HOST, port))
    server.listen(5)
    return server


def dev_accept_loop(server):
    """Accepts the development connection: one at a time; others are told so and closed."""
    while not _stop.isSet():
        try:
            conn, addr = server.accept()
        except Exception:
            return  # the server socket was closed: the bridge stops
        if not _dev_slot.acquire(False):
            reject_client(conn, DEV_BUSY, allow_shutdown=True)
            continue
        print("Development client connected (Laertes is blocked until it disconnects):", addr)
        t = threading.Thread(target=serve_client, args=(conn, _dev_slot, True))
        t.setDaemon(True)
        t.start()


def main():
    server = listen(PORT)
    print("ODIS Jython bridge listening on %s:%d" % (HOST, PORT))
    dev_server = listen(DEV_PORT)
    print("Development connection on %s:%d" % (HOST, DEV_PORT))
    t = threading.Thread(target=dev_accept_loop, args=(dev_server,))
    t.setDaemon(True)
    t.start()

    try:
        while not _stop.isSet():
            conn, addr = server.accept()
            if _stop.isSet():
                conn.close()
                break
            if not _client_slot.acquire(False):
                reject_client(conn)
                continue
            print("Client connected:", addr)
            # Daemon threads: a stopped bridge must not keep the macro running.
            t = threading.Thread(target=serve_client, args=(conn,))
            t.setDaemon(True)
            t.start()
    finally:
        server.close()
        dev_server.close()
    print("ODIS bridge stopped; ODIS can be used again.")


main()
