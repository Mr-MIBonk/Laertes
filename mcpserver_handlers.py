# C:\ProgramData\OE\macros\mcpserver_handlers.py
# Actions of the ODIS Jython TCP bridge. mcpserver_test.py runs the TCP server and reloads
# this file whenever it changes, so actions can be added or fixed without restarting ODIS.
#
# Every handler takes the request dict and returns a JSON-serializable dict with "ok".
# DIAG_LOCK is provided by mcpserver_test.py; it serializes access to the control units.
# Do NOT put MCP server code in this file.

import json
import re
import traceback

from org.osgi.framework import FrameworkUtil
from java.lang import Boolean, Character, Enum, Number, Throwable
from java.util import Collection, Map
from de.volkswagen.odis.vaudas.vehiclefunction.automation.IDiagnosticInterface import Factory


# --- ODIS services ------------------------------------------------------------------------

def get_context():
    diag = Factory.getInstance()
    bundle = FrameworkUtil.getBundle(diag.getClass())
    return bundle.getBundleContext()


def get_entrance(context):
    ref = context.getServiceReference(
        "de.volkswagen.odis.vaudas.diag.systemtest.IDiagEntranceService"
    )

    if ref is None:
        raise Exception("IDiagEntranceService not found")

    return context.getService(ref), ref


def get_uds_service(context):
    ref = context.getServiceReference(
        "de.volkswagen.odis.vaudas.diag.ecuonboard.uds.IEcuOnBoardDiagJobServiceUds"
    )

    if ref is None:
        raise Exception("IEcuOnBoardDiagJobServiceUds not found")

    return context.getService(ref), ref


def get_ecu(index):
    context = get_context()
    entrance, entrance_ref = get_entrance(context)

    try:
        plan = entrance.createProjectEcuPlan()
        ecus = plan.getEcuList()

        index = int(index)

        if index < 0 or index >= len(ecus):
            raise Exception("ECU index out of range: %d" % index)

        return ecus.get(index)

    finally:
        context.ungetService(entrance_ref)


def java_object_info(obj, include_methods=True):
    result = {
        "repr": str(obj)
    }

    try:
        clazz = obj.getClass()
        result["class"] = clazz.getName()

        if include_methods:
            methods = []

            for method in clazz.getMethods():
                methods.append(method.getName())

            result["methods"] = sorted(set(methods))

    except Exception as e:
        result["error"] = str(e)

    return result


# --- Converting API results to JSON -------------------------------------------------------

MAX_DEPTH = 6


def to_data(obj, depth=0, seen=None):
    """Turns a result of the ODIS automation API into plain JSON data.

    Java beans become dicts of their get*/is* properties, collections become lists,
    byte arrays become hex strings. Recursion stops at MAX_DEPTH and on cycles.
    """
    if obj is None or isinstance(obj, (bool, int, long, float, basestring)):
        return obj
    if isinstance(obj, (Boolean, Number, Character)):
        return obj.toString() if isinstance(obj, Character) else obj
    if isinstance(obj, Enum):
        return obj.name()
    if hasattr(obj, "typecode") and getattr(obj, "typecode", None) == "b":  # Java byte[]
        return "".join("%02X" % (b & 0xFF) for b in obj)
    if isinstance(obj, (list, tuple)):
        return [to_data(x, depth + 1, seen) for x in obj]
    if isinstance(obj, Map):
        return dict((unicode(k), to_data(obj.get(k), depth + 1, seen)) for k in obj.keySet())
    if isinstance(obj, Collection):
        return [to_data(x, depth + 1, seen) for x in obj]
    if depth >= MAX_DEPTH:
        return unicode(obj)

    seen = set(seen or ())
    if id(obj) in seen:
        return unicode(obj)
    seen.add(id(obj))

    try:
        clazz = obj.getClass()
    except Exception:
        return unicode(obj)

    result = {"_type": clazz.getSimpleName()}
    names = set()
    for method in clazz.getMethods():
        name = method.getName()
        if len(method.getParameterTypes()) != 0 or name in ("getClass", "hashCode", "toString"):
            continue
        if name.startswith("get") and len(name) > 3:
            names.add((name, name[3].lower() + name[4:]))
        elif name.startswith("is") and len(name) > 2:
            names.add((name, name[2].lower() + name[3:]))
    for name, prop in sorted(names):
        try:
            result[prop] = to_data(getattr(obj, name)(), depth + 1, seen)
        except Exception as e:
            result[prop] = "<error: %s>" % e
    if len(result) == 1:
        result["value"] = unicode(obj)
    return result


# --- Connections to control units ---------------------------------------------------------

def parse_address(cmd):
    address = cmd.get("address")
    if address is None:
        raise Exception("missing address (control unit address, e.g. \"0x0001\")")
    if isinstance(address, basestring):
        return int(address, 0)
    return int(address)


def with_connection(cmd, fn):
    """Selects the control unit at cmd["address"] and returns fn(diag, handle).

    connectToEcu leaves ODIS in its default behaviour, where every diagnostic operation opens
    and closes the link itself, so there is nothing to close afterwards (closeConnection is
    only allowed after connectToEcuAndOpenConnection).
    """
    address = parse_address(cmd)
    diag = Factory.getInstance()
    DIAG_LOCK.acquire()
    try:
        handle = diag.connectToEcu(address).getConnectionHandle()
        return checked(fn(diag, handle))
    except (Exception, Throwable) as e:
        traceback.print_exc()
        return {"ok": False, "error": error_text(e), "exception_type": e.__class__.__name__}
    finally:
        DIAG_LOCK.release()


def error_text(e):
    """Message of a Python or Java exception; ODIS messages may contain non-ASCII text."""
    if isinstance(e, Throwable):
        return unicode(e.getMessage() or e.toString())
    try:
        return unicode(e)
    except Exception:
        return repr(e)


def checked(result):
    """Returns result, or an error result if it cannot be sent as JSON."""
    try:
        json.dumps(result, default=unicode)
        return result
    except Exception as e:
        traceback.print_exc()
        return {"ok": False, "error": "cannot encode result as JSON: %s: %s" % (e.__class__.__name__, e)}


def names_result(names, **extra):
    names = [unicode(n) for n in (names or [])]
    result = {"ok": True, "count": len(names), "names": names}
    result.update(extra)
    return result


# --- Original actions ---------------------------------------------------------------------

def handle_list_project_ecus(cmd):
    context = get_context()
    entrance, entrance_ref = get_entrance(context)

    try:
        plan = entrance.createProjectEcuPlan()
        ecus = plan.getEcuList()
        include_methods = bool(cmd.get("include_methods", True))

        result = []

        for i in range(len(ecus)):
            ecu = ecus.get(i)
            info = java_object_info(ecu, include_methods)
            info["index"] = i
            result.append(info)

        return {
            "ok": True,
            "count": len(result),
            "ecus": result
        }

    finally:
        context.ungetService(entrance_ref)


def session_vehicle():
    """The vehicle of the current ODIS diagnostic session (holds the units ODIS identified)."""
    context = get_context()
    ref = context.getServiceReference("de.volkswagen.odis.vaudas.session.ISessionVehicleDataProvider")
    if ref is None:
        raise Exception("ISessionVehicleDataProvider not found")
    try:
        holder = context.getService(ref).supplyVehicle()
        # The holder's class is not public, so its getter is called by reflection.
        from java.lang import Class
        method = Class.getMethod(holder.getClass(), "getVehicleAsVaudasVehicle", [])
        method.setAccessible(True)
        return method.invoke(holder, [])
    finally:
        context.ungetService(ref)


def ecu_fields(ecu):
    """What ODIS currently knows about a unit; "---" where it has not identified it."""
    out = {"repr": unicode(ecu)}
    for key, getter in (("hw_part", "getHWPartNumber"), ("sw_part", "getSWPartNumber"),
                        ("hw_version", "getHWVersion"), ("sw_version", "getSWVersion"),
                        ("protocol", "getProtocol"), ("odx_variant", "getOdxVariant"),
                        ("read_state", "getReadState"), ("available", "isAvailable"),
                        ("system_name", "getSystemIdentification"), ("serial", "getSerialNumber")):
        try:
            out[key] = to_data(getattr(ecu, getter)(), MAX_DEPTH - 1)
        except (Exception, Throwable):
            out[key] = None
    try:  # stored DTCs, if ODIS has read the event memory of this unit
        out["dtc_count"] = ecu.getActualDTCs().size() if ecu.isDtcRead() else None
    except (Exception, Throwable):
        out["dtc_count"] = None
    return out


def handle_list_vehicle_ecus(cmd):
    try:
        return checked({"ok": True, "ecus": [ecu_fields(e) for e in session_vehicle().getActualECUs()]})
    except (Exception, Throwable) as e:
        traceback.print_exc()
        return {"ok": False, "error": error_text(e)}


def handle_find_ecu(cmd):
    name = cmd.get("name", "")
    context = get_context()
    entrance, entrance_ref = get_entrance(context)

    try:
        plan = entrance.createProjectEcuPlan()
        ecus = plan.getEcuList()

        matches = []

        for i in range(len(ecus)):
            ecu = ecus.get(i)
            text = str(ecu)

            if name.lower() in text.lower():
                info = java_object_info(ecu)
                info["index"] = i
                matches.append(info)

        return {
            "ok": True,
            "query": name,
            "count": len(matches),
            "matches": matches
        }

    finally:
        context.ungetService(entrance_ref)


def handle_get_ecu_coding_info(cmd):
    ecu = get_ecu(cmd.get("index", 0))
    info = java_object_info(ecu)

    coding_info = None

    try:
        coding_info = ecu.getCodingInfo()
    except Exception as e:
        info["coding_info_error"] = str(e)

    if coding_info is not None:
        info["coding_info"] = java_object_info(coding_info)

    return {
        "ok": True,
        "ecu": info
    }


def handle_create_coding_job(cmd):
    context = get_context()
    entrance, entrance_ref = get_entrance(context)
    uds_service, uds_ref = get_uds_service(context)

    try:
        plan = entrance.createProjectEcuPlan()
        ecus = plan.getEcuList()

        index = int(cmd.get("index", 0))

        if index < 0 or index >= len(ecus):
            return {"ok": False, "error": "ECU index out of range"}

        ecu = ecus.get(index)
        job = uds_service.createCoding(ecu)

        if job is None:
            return {
                "ok": True,
                "ecu": str(ecu),
                "job": None
            }

        return {
            "ok": True,
            "ecu": str(ecu),
            "job": java_object_info(job)
        }

    finally:
        context.ungetService(entrance_ref)
        context.ungetService(uds_ref)


def handle_get_coding_items(cmd):
    context = get_context()
    entrance, entrance_ref = get_entrance(context)
    uds_service, uds_ref = get_uds_service(context)

    try:
        plan = entrance.createProjectEcuPlan()
        ecus = plan.getEcuList()

        index = int(cmd.get("index", 0))

        if index < 0 or index >= len(ecus):
            return {"ok": False, "error": "ECU index out of range"}

        ecu = ecus.get(index)
        job = uds_service.createCoding(ecu)

        if job is None:
            return {
                "ok": True,
                "ecu": str(ecu),
                "items": []
            }

        items = job.getItems()
        result = []

        for item in items:
            result.append(java_object_info(item))

        return {
            "ok": True,
            "ecu": str(ecu),
            "count": len(result),
            "items": result
        }

    finally:
        context.ungetService(entrance_ref)
        context.ungetService(uds_ref)


def handle_get_coding_structure(cmd):
    context = get_context()
    entrance, entrance_ref = get_entrance(context)
    uds_service, uds_ref = get_uds_service(context)

    try:
        plan = entrance.createProjectEcuPlan()
        ecus = plan.getEcuList()

        index = int(cmd.get("index", 0))

        if index < 0 or index >= len(ecus):
            return {"ok": False, "error": "ECU index out of range"}

        ecu = ecus.get(index)
        job = uds_service.createCoding(ecu)

        if job is None:
            return {
                "ok": True,
                "ecu": str(ecu),
                "identified_systems": [],
                "text_values": []
            }

        systems = job.getIdentifiedSystems()
        values = job.getTextCodingValues()

        systems_data = []
        values_data = []

        for system in systems:
            systems_data.append(java_object_info(system))

        for value in values:
            values_data.append(java_object_info(value))

        return {
            "ok": True,
            "ecu": str(ecu),
            "job_class": job.getClass().getName(),
            "identified_system_count": len(systems_data),
            "identified_systems": systems_data,
            "text_value_count": len(values_data),
            "text_values": values_data
        }

    finally:
        context.ungetService(entrance_ref)
        context.ungetService(uds_ref)


def handle_read_coding_values(cmd):
    context = get_context()
    entrance, entrance_ref = get_entrance(context)
    uds_service, uds_ref = get_uds_service(context)

    try:
        plan = entrance.createProjectEcuPlan()
        ecus = plan.getEcuList()

        index = int(cmd.get("index", 0))

        if index < 0 or index >= len(ecus):
            return {"ok": False, "error": "ECU index out of range"}

        ecu = ecus.get(index)
        job = uds_service.createCoding(ecu)

        if job is None:
            return {"ok": False, "error": "createCoding returned null"}

        nrc_handling = bool(cmd.get("nrc_handling", False))

        job.readCodingValues(nrc_handling)

        items = job.getItems()
        values = job.getTextCodingValues()
        systems = job.getIdentifiedSystems()

        items_data = []

        for item in items:
            items_data.append(java_object_info(item))

        return {
            "ok": True,
            "ecu": str(ecu),
            "read_state": str(job.getByteReadState()),
            "item_count": len(items_data),
            "text_value_count": len(values),
            "identified_system_count": len(systems),
            "items": items_data
        }

    finally:
        context.ungetService(entrance_ref)
        context.ungetService(uds_ref)


def handle_ping(cmd):
    return {
        "ok": True,
        "pong": True,
        "bridge": "odis-jython-tcp",
        "actions": sorted(HANDLERS.keys())
    }


# --- Control unit data through the ODIS automation API (IDiagnosticInterface) ---------------
# These take "address", the control unit address (e.g. "0x0001" or 1), and connect to the unit.

def handle_read_identification(cmd):
    return with_connection(cmd, lambda diag, h: {
        "ok": True, "identification": to_data(diag.readIdentification(h))})


def handle_read_coding(cmd):
    # The first entry is the master's coding, then one per slave/subsystem.
    return with_connection(cmd, lambda diag, h: {
        "ok": True, "coding": to_data(diag.readCoding(h))})


def handle_list_coding_values(cmd):
    return with_connection(cmd, lambda diag, h: names_result(diag.getShortNamesOfCodingValues(h)))


def handle_list_adaptation_channels(cmd):
    return with_connection(cmd, lambda diag, h: names_result(diag.getShortNamesOfAdaptationChannels(h)))


def handle_read_adaptation(cmd):
    channel = cmd.get("channel")
    if not channel:
        return {"ok": False, "error": "missing channel"}
    return with_connection(cmd, lambda diag, h: {
        "ok": True, "channel": channel, "adaptation": to_data(diag.readAdaptation(h, channel))})


def handle_list_measurement_groups(cmd):
    return with_connection(cmd, lambda diag, h: names_result(diag.getShortNamesOfMeasurementGroups(h)))


def handle_list_measurements(cmd):
    group = cmd.get("group")
    if not group:
        return {"ok": False, "error": "missing group"}
    return with_connection(cmd, lambda diag, h: names_result(
        diag.getShortNamesOfMeasurements(h, group), group=group))


def handle_read_measurement(cmd):
    group, measurement = cmd.get("group"), cmd.get("measurement")
    if not group or not measurement:
        return {"ok": False, "error": "missing group or measurement"}
    return with_connection(cmd, lambda diag, h: {
        "ok": True, "group": group, "measurement": measurement,
        "values": to_data(diag.readMeasurement(h, group, measurement))})


def handle_read_measurement_group(cmd):
    group = cmd.get("group")
    if not group:
        return {"ok": False, "error": "missing group"}

    def read(diag, h):
        values = []
        for name in diag.getShortNamesOfMeasurements(h, group):
            try:
                values.append(to_data(diag.readMeasurement(h, group, name)))
            except (Exception, Throwable) as e:
                values.append({"measurement": unicode(name), "error": error_text(e)})
        return {"ok": True, "group": group, "values": values}
    return with_connection(cmd, read)


def handle_read_gateway_component_list(cmd):
    # The gateway's list of control units installed in the vehicle; address defaults to 0x0019.
    cmd = dict(cmd)
    cmd.setdefault("address", "0x0019")
    return with_connection(cmd, lambda diag, h: {
        "ok": True, "components": to_data(diag.readGatewayComponentList(h))})


def handle_clear_event_memory(cmd):
    # Deletes the unit's stored DTCs, then reads the event memory again to show what is left.
    def clear(diag, h):
        diag.resetEventMemory(h)
        return {"ok": True, "cleared": True, "event_memory": to_data(diag.readEventMemory(h))}
    return with_connection(cmd, clear)


def handle_read_event_memory(cmd):
    return with_connection(cmd, lambda diag, h: {
        "ok": True, "event_memory": to_data(diag.readEventMemory(h))})


# --- ODIS database (ODX via ASAM MCD-3D), no connection to the vehicle --------------------
# ODIS keeps its ODX data in an MCD project, reached through the ECU communication framework.

def read_field(obj, name):
    """Value of a (possibly private) field; ODIS exposes the MCD server only through one."""
    from java.lang import Class
    clazz = obj.getClass()
    while clazz is not None:
        for f in Class.getDeclaredFields(clazz):
            if f.getName() == name:
                f.setAccessible(True)
                return f.get(obj)
        clazz = Class.getSuperclass(clazz)
    raise Exception("no field %s" % name)


def mcd_project():
    context = get_context()
    ref = context.getServiceReference("de.volkswagen.odis.vaudas.ecucomframework.IEcuComMngmtService")
    if ref is None:
        raise Exception("IEcuComMngmtService not found")
    try:
        framework = read_field(context.getService(ref).getInstance(), "framework")
        return framework.getMcdServer().getMcdProject()
    finally:
        context.ungetService(ref)


def mcd_items(collection):
    for i in range(collection.getCount()):
        yield collection.getItemByIndex(i)


def project_ecu_name(address):
    """Short name of the project control unit with this address, e.g. "EnginContrModul1"."""
    context = get_context()
    entrance, entrance_ref = get_entrance(context)
    try:
        for ecu in entrance.createProjectEcuPlan().getEcuList():
            m = re.search(r"id\[(0x[0-9A-Fa-f]+)\] sname\[([^\]]*)\]", unicode(ecu))
            if m and int(m.group(1), 16) == address:
                return m.group(2)
    finally:
        context.ungetService(entrance_ref)
    raise Exception("no control unit with address 0x%04X in the ODIS project" % address)


def odx_link(cmd):
    """The database logical link of the unit at cmd["address"] (UDS preferred)."""
    name = project_ecu_name(parse_address(cmd))
    links = mcd_project().getActiveDbVehicleInformation().getDbLogicalLinks()
    candidates = [n for n in links.getNames() if n.startswith("LL_" + name)]
    if not candidates:
        raise Exception("no logical link for %s in the ODIS database" % name)
    candidates.sort(key=lambda n: (not n.endswith("UDS"), n))
    return links.getItemByName(candidates[0])


def odx_location(cmd):
    """Database location to read: the ECU variant cmd["variant"] (with its base variant
    cmd["base"]) if given, otherwise the base variant of the unit at cmd["address"]."""
    variant = cmd.get("variant")
    if not variant:
        link = odx_link(cmd)
        return link.getDbLocation(), {"logical_link": link.getShortName(), "long_name": link.getLongName(),
                                      "protocol": link.getProtocolType()}
    bases = mcd_project().getDbProject().getDbEcuBaseVariants()
    names = [cmd["base"]] if cmd.get("base") else list(bases.getNames())
    for base_name in names:
        base = bases.getItemByName(base_name)
        if variant in list(base.getDbEcuVariants().getNames()):
            location = base.getDbEcuVariants().getItemByName(variant).getDbLocations().getItemByIndex(0)
            return location, {"base": base_name, "variant": variant}
    raise Exception("no ECU variant %s in the ODIS database" % variant)


def handle_odx_variants(cmd):
    """ECU variants in the database: those of the unit at "address", or every variant whose name
    contains "query" (e.g. a part number such as 04C906026BQ)."""
    def read():
        bases = mcd_project().getDbProject().getDbEcuBaseVariants()
        if cmd.get("address") is not None:
            name = project_ecu_name(parse_address(cmd))
            base_names = [n for n in bases.getNames() if n.startswith("BV_" + name)]
        else:
            base_names = list(bases.getNames())
        query = (cmd.get("query") or "").lower()
        out = []
        for base_name in base_names:
            for v in bases.getItemByName(base_name).getDbEcuVariants().getNames():
                if query in v.lower():
                    out.append({"base": base_name, "variant": v})
        return {"ok": True, "count": len(out), "variants": out[:5000]}
    return with_database(read)


def with_database(fn):
    DIAG_LOCK.acquire()
    try:
        return checked(fn())
    except (Exception, Throwable) as e:
        traceback.print_exc()
        return {"ok": False, "error": error_text(e), "exception_type": e.__class__.__name__}
    finally:
        DIAG_LOCK.release()


def mcd_value(value):
    try:
        return unicode(value.getValueAsString())
    except Exception:
        return None


def handle_odx_unit(cmd):
    def read():
        location, info = odx_location(cmd)
        tables = [{"name": t.getShortName(), "long_name": t.getLongName(), "semantic": t.getSemantic(),
                   "rows": t.getDbTableRows().getCount()} for t in mcd_items(location.getDbTables())]
        result = {"ok": True, "location": location.getShortName(),
                  "location_long_name": location.getLongName(),
                  "dtc_count": location.getDbDTCs(255, 1).getCount(),
                  "tables": sorted(tables, key=lambda t: t["name"])}
        result.update(info)
        return result
    return with_database(read)


def handle_odx_dtcs(cmd):
    # getDbDTCs takes the severity levels as (highest, lowest), each 1..255.
    def read():
        location, info = odx_location(cmd)
        dtcs = [{"code": d.getDisplayTroubleCode(), "number": d.getTroubleCode(),
                 "text": d.getUnicodeDTCText() or d.getDTCText(), "level": d.getLevel()}
                for d in mcd_items(location.getDbDTCs(255, 1))]
        dtcs.sort(key=lambda d: d["number"])
        result = {"ok": True, "location": location.getShortName(), "count": len(dtcs), "dtcs": dtcs}
        result.update(info)
        return result
    return with_database(read)


def handle_odx_table(cmd):
    """Rows of a database table (e.g. TAB_RecorDataIdentCalibData = adaptations), with their
    parameters, units and selectable values."""
    table_name = cmd.get("table")
    if not table_name:
        return {"ok": False, "error": "missing table"}

    def param(p):
        # Each detail is optional: e.g. reserved parameters have no data definition at all.
        out = {"name": p.getShortName()}
        for key, get in (("long_name", p.getLongName), ("unit", p.getUnit)):
            try:
                out[key] = get() or ""
            except (Exception, Throwable):
                pass
        # Where the value sits in the record, so a coding can be decoded with this definition.
        for key, get in (("byte_pos", p.getBytePos), ("bit_pos", p.getBitPos), ("bit_length", p.getBitLength)):
            try:
                out[key] = int(get())
            except (Exception, Throwable):
                pass
        try:
            elements = list(mcd_items(p.getTextTableElements()))
            out["choices"] = [e.getLongName() for e in elements]
            out["values"] = [{"text": e.getLongName(), "low": mcd_value(e.getInterval().getLowerLimit()),
                              "high": mcd_value(e.getInterval().getUpperLimit())} for e in elements]
        except (Exception, Throwable):
            pass
        return out

    def read():
        location, info = odx_location(cmd)
        table = location.getDbTables().getItemByName(table_name)
        rows = []
        for row in mcd_items(table.getDbTableRows()):
            rows.append({"name": row.getShortName(), "long_name": row.getLongName(),
                         "key": mcd_value(row.getKey()),
                         "params": [param(p) for p in mcd_items(row.getDbParameters())]})
        result = {"ok": True, "location": location.getShortName(), "table": table_name,
                  "long_name": table.getLongName(), "count": len(rows), "rows": rows}
        result.update(info)
        return result
    return with_database(read)


TEXTUAL_TABLE = "TAB_RecorDataIdentVariaCodinWritaTextu"
_textual_cache = {}  # base variant -> its variants that define textual coding (reset on reload)


def textual_params(location):
    """Short names of the textual coding parameters a database location defines."""
    table = location.getDbTables().getItemByName(TEXTUAL_TABLE)
    return [p.getShortName() for row in mcd_items(table.getDbTableRows()) for p in mcd_items(row.getDbParameters())]


def handle_odx_textual_variants(cmd):
    """Variants of a unit's family (same base variant) that define textual coding, closest names to
    cmd["variant"] first. The family is given by "base" or by the unit "address"."""
    def read():
        bases = mcd_project().getDbProject().getDbEcuBaseVariants()
        if cmd.get("base"):
            base_names = [cmd["base"]]
        else:
            name = project_ecu_name(parse_address(cmd))
            base_names = [n for n in bases.getNames() if n.startswith("BV_" + name)]
        found = []
        for base_name in base_names:
            if base_name not in _textual_cache:
                hits = []
                for v in mcd_items(bases.getItemByName(base_name).getDbEcuVariants()):
                    try:
                        params = textual_params(v.getDbLocations().getItemByIndex(0))
                    except (Exception, Throwable):
                        continue  # no textual coding table in this variant
                    if len(params) > 1:  # one parameter is "Disabled" or a plain byte field
                        hits.append({"base": base_name, "variant": v.getShortName(), "params": len(params)})
                _textual_cache[base_name] = hits
            found += _textual_cache[base_name]
        def family(name):  # GV_/EV_ prefixes differ between otherwise related variants
            return re.sub(r"^[A-Z]V_", "", name)
        ref = family(cmd.get("variant") or "")

        def common(name):
            name = family(name)
            n = 0
            while n < min(len(name), len(ref)) and name[n] == ref[n]:
                n += 1
            return n
        found.sort(key=lambda x: (-common(x["variant"]), x["variant"]))
        return {"ok": True, "count": len(found), "variants": found}
    return with_database(read)


# --- Exploration (debug) ------------------------------------------------------------------
# _debug calls methods inside ODIS by name, so it requires the token from DEBUG_TOKEN_FILE:
# local tools can read that file, a web page that posts to this port cannot.

DEBUG_TOKEN_FILE = r"C:\ProgramData\OE\macros\.mcpserver_debug_token"


def debug_token():
    import os, binascii
    if not os.path.exists(DEBUG_TOKEN_FILE):
        f = open(DEBUG_TOKEN_FILE, "w")
        f.write(binascii.hexlify(os.urandom(16)))
        f.close()
    return open(DEBUG_TOKEN_FILE).read().strip()


def invoke_declared(obj, name):
    from java.lang import Class
    clazz = obj.getClass()
    while clazz is not None:
        for m in Class.getDeclaredMethods(clazz):
            if m.getName() == name and len(m.getParameterTypes()) == 0:
                m.setAccessible(True)
                return m.invoke(obj, [])
        clazz = Class.getSuperclass(clazz)
    raise Exception("no method %s()" % name)


def field_list(clazz):
    from java.lang import Class
    out = []
    while clazz is not None:
        out += [re.sub(r"[\w$]+\.", "", f.toString()) for f in Class.getDeclaredFields(clazz)]
        clazz = Class.getSuperclass(clazz)
    return out


def handle__debug(cmd):
    """Explores ODIS objects: {"token", "service", "path": [step, ...], "param_of": [method, i]}.

    A step is a method name (no arguments), [name, arg, ...], an int (element of a collection),
    "@pkg.Class.method" (static method of a class loaded through the current object's bundle)
    "!method" (no-argument method called by reflection, for non-public classes), ".field"
    (field read by reflection)
    or, as the last step, "#pkg.Class" (list that class's methods, static ones included).
    Returns the class, text and public methods of the object reached.
    """
    if cmd.get("token") != debug_token():
        return {"ok": False, "error": "missing or wrong debug token (see %s)" % DEBUG_TOKEN_FILE}
    from java.lang import Class
    context = get_context()
    if cmd.get("list_services") is not None:  # names of registered services containing this text
        q = cmd["list_services"].lower()
        names = set()
        for r in context.getAllServiceReferences(None, None) or []:
            names.update(n for n in r.getProperty("objectClass") if q in n.lower())
        return {"ok": True, "services": sorted(names)}
    ref = context.getServiceReference(cmd["service"])
    obj = context.getService(ref)
    try:
        listed = None
        for step in cmd.get("path", []):
            if isinstance(step, basestring) and step.startswith("#"):
                listed = FrameworkUtil.getBundle(obj.getClass()).loadClass(step[1:])
            elif isinstance(step, basestring) and step.startswith("."):
                obj = read_field(obj, step[1:])
            elif isinstance(step, basestring) and step.startswith("!"):
                obj = invoke_declared(obj, step[1:])
            elif isinstance(step, basestring) and step.startswith("@"):
                cls_name, meth = step[1:].rsplit(".", 1)
                clazz = FrameworkUtil.getBundle(obj.getClass()).loadClass(cls_name)
                obj = Class.getMethod(clazz, meth, []).invoke(None, [])
            elif isinstance(step, int):
                obj = list(obj)[step]
            elif isinstance(step, list):
                obj = getattr(obj, step[0])(*step[1:])
            else:
                obj = getattr(obj, step)()
        clazz = listed or (obj.getClass() if obj is not None else None)
        if cmd.get("param_of"):
            name, i = cmd["param_of"]
            clazz = [m for m in clazz.getMethods() if m.getName() == name][0].getParameterTypes()[i]
        sigs = []
        if clazz is not None:
            for m in (Class.getDeclaredMethods(clazz) if listed else Class.getMethods(clazz)):
                sig = re.sub(r"[\w$]+\.", "", m.toString().replace("public ", "").replace("abstract ", ""))
                if not re.search(r" (wait|notify|notifyAll|hashCode|equals|getClass|toString)\(", sig):
                    sigs.append(sig)
        size = None
        try:
            size = obj.size()
        except Exception:
            try:
                size = obj.getCount()
            except Exception:
                pass
        if cmd.get("names"):  # sample of an MCD collection's names
            return {"ok": True, "size": size, "names": list(obj.getNames())[:cmd["names"]]}
        return {"ok": True, "class": Class.getName(clazz) if clazz is not None else None,
                "repr": unicode(obj)[:500], "size": size, "methods": sorted(set(sigs)),
                "fields": field_list(clazz) if clazz is not None else []}
    except (Exception, Throwable) as e:
        traceback.print_exc()
        return {"ok": False, "error": error_text(e)}
    finally:
        context.ungetService(ref)


# --- Writing coding and adaptation, security access ---------------------------------------
# These change the control unit. The web UI confirms each change (old -> new) before calling.

def with_open_connection(cmd, fn):
    """Like with_connection, but keeps one link open for the whole call so a security-access
    login and the operation that needs it run on the same connection. ODIS otherwise opens and
    closes a fresh link per operation, which drops the unlock before the write."""
    from de.volkswagen.odis.vaudas.vehiclefunction.automation.IDiagnosticInterface import Factory
    address = parse_address(cmd)
    diag = Factory.getInstance()
    handle = diag.connectToEcu(address).getConnectionHandle()
    diag.openConnection(handle)
    try:
        return checked(fn(diag, handle))
    finally:
        try:
            diag.closeConnection(handle)
        except (Exception, Throwable):
            pass


def maybe_login(diag, handle, cmd):
    """On an open connection: optional diagnostic-session switch, then security access, when the
    command carries a login value. Lets a write unlock the unit on its own connection."""
    session = cmd.get("session")
    if session:
        diag.switchSession(handle, session)
    code = cmd.get("login")
    if code is not None:
        mode = cmd.get("mode")
        if not mode:
            modes = list(diag.getShortNamesOfAccessModes(handle))
            mode = modes[0] if modes else ""
        diag.securityAccess(handle, unicode(code), mode)


def gated_connection(cmd):
    """with_open_connection when a login/session is requested (so the unlock holds through the
    write), otherwise the default per-operation with_connection."""
    return with_open_connection if (cmd.get("login") is not None or cmd.get("session")) else with_connection


def handle_write_byte_coding(cmd):
    """Writes byte coding to a subsystem. name is the subsystem (systemName from read_coding,
    e.g. "master"); coding is the hex string in the same form read_coding returns. An optional
    login (and optional mode/session) performs security access on the same connection first."""
    name = cmd.get("name") or "master"
    coding = cmd.get("coding")
    if not coding:
        return {"ok": False, "error": "missing coding hex string"}
    def run(diag, h):
        maybe_login(diag, h, cmd)
        return {"ok": True, "result": to_data(diag.writeByteCoding(h, name, coding))}
    return gated_connection(cmd)(cmd, run)


def handle_read_adaptation_values(cmd):
    """The selectable values of an adaptation channel, when it has a fixed set."""
    channel = cmd.get("channel")
    if not channel:
        return {"ok": False, "error": "missing channel"}
    return with_connection(cmd, lambda diag, h: names_result(diag.getShortNamesOfAdaptationValues(h, channel), channel=channel))


def handle_adaptation_options(cmd):
    """Enumerated options for an adaptation channel's parameters, from the writable calibration
    ODX table (matched by the channel's long name). Lets the editor offer dropdowns where a
    parameter has a fixed set of values. Resolved by address, so no vehicle session is needed."""
    channel = cmd.get("channel")
    if not channel:
        return {"ok": False, "error": "missing channel"}
    table = handle_odx_table({"address": cmd.get("address"), "variant": cmd.get("variant"),
                              "base": cmd.get("base"), "table": "TAB_RecorDataIdentCalibDataWrita"})
    if not table.get("ok"):
        return {"ok": True, "channel": channel, "params": []}
    for row in table.get("rows", []):
        if row.get("long_name") == channel or row.get("name") == channel:
            params = []
            for p in row.get("params", []):
                opts = [c.get("text") for c in (p.get("values") or []) if c.get("low") == c.get("high")]
                if opts:
                    params.append({"name": p.get("name"), "options": opts})
            return {"ok": True, "channel": channel, "params": params}
    return {"ok": True, "channel": channel, "params": []}


def handle_write_adaptation(cmd):
    """Writes an adaptation channel. values maps each parameter name (as read_adaptation shows,
    e.g. "Param_ActivStartStopFunct") to its new value."""
    from java.util import HashMap
    channel, values = cmd.get("channel"), cmd.get("values")
    if not channel or not isinstance(values, dict) or not values:
        return {"ok": False, "error": "missing channel or non-empty values map"}
    m = HashMap()
    for k, v in values.items():
        m.put(k, v)
    def run(diag, h):
        maybe_login(diag, h, cmd)
        return {"ok": True, "result": to_data(diag.writeAdaptation(h, channel, m))}
    return gated_connection(cmd)(cmd, run)


def handle_list_access_modes(cmd):
    """The named security-access modes an ECU offers (for the login value)."""
    return with_connection(cmd, lambda diag, h: names_result(diag.getShortNamesOfAccessModes(h)))


def handle_login(cmd):
    """Security access ("login"): elevated mode for coding/adaptation. securityAccess takes an
    access-mode name and a value; mode defaults to the first the ECU offers."""
    value = cmd.get("code")
    if value is None:
        value = cmd.get("value") or ""
    def run(diag, h):
        session = cmd.get("session")
        if session:
            diag.switchSession(h, session)
        modes = list(diag.getShortNamesOfAccessModes(h))
        mode = cmd.get("mode") or (modes[0] if modes else "")
        return {"ok": True, "mode": mode, "modes": [unicode(x) for x in modes],
                "result": to_data(diag.securityAccess(h, unicode(value), mode))}
    # Open the link for the whole call; note the unlock is dropped when it closes, so a coding or
    # adaptation write must carry the login itself to be unlocked on its own connection.
    return with_open_connection(cmd, run)


# --- Guided fault finding (GFS) -----------------------------------------------------------
# ODIS-S guided fault finding ("Gefuehrte Fehlersuche") hangs off an IGfs session, created for
# the vehicle and held by GfsManager. These actions report its state and, from it, the repair
# guidance (test programs, documents) for a fault, so a DTC can be shown with what to check.

def gfs_methods(obj):
    from java.lang import Class
    if obj is None:
        return None
    sigs = []
    for m in Class.getMethods(obj.getClass()):
        sig = re.sub(r"[\w$]+\.", "", m.toString().replace("public ", "").replace("abstract ", "").replace("final ", ""))
        if not re.search(r" (wait|notify|notifyAll|hashCode|equals|getClass|toString)\(", sig):
            sigs.append(sig)
    return sorted(set(sigs))


def gfs_service(context, name):
    ref = context.getServiceReference(name)
    return (context.getService(ref) if ref is not None else None), ref


def current_gfs(context):
    """The current IGfs guided-diagnosis session, or None. getGfs() is static on GfsManager
    (the ISessionGfsDataProvider impl), so it is called by reflection on that class."""
    from java.lang import Class
    mgr, ref = gfs_service(context, "de.volkswagen.odis.vaudas.session.ISessionGfsDataProvider")
    try:
        if mgr is None:
            return None
        return Class.getMethod(mgr.getClass(), "getGfs", []).invoke(None, [])
    finally:
        if ref is not None:
            context.ungetService(ref)


def handle_fault_guidance(cmd):
    """Guided-fault-finding guidance for a fault: repair documents and diagnostic objects the
    GFS search service finds for the query (a DTC code or text). Needs an active guided-diagnosis
    session (vehicle identified and Guided Fault Finding started in ODIS); the knowledge base is
    empty otherwise."""
    query = cmd.get("query") or cmd.get("dtc") or ""
    context = get_context()
    gfs = current_gfs(context)
    if gfs is None:
        return {"ok": False, "error": "No guided diagnosis session; identify the vehicle and start Guided Fault Finding in ODIS."}
    out = {"ok": True, "query": query, "gfs_class": gfs.getClass().getName()}
    search, ref = gfs_service(context, "de.volkswagen.odis.vaudas.gfs.search.IGfsSearchService")
    try:
        try:
            out["documents"] = [unicode(x)[:300] for x in (search.findDocLinks(gfs, query, True, True) or [])]
        except (Exception, Throwable) as e:
            out["documents_error"] = error_text(e)
        try:
            out["diagnostic_objects"] = [unicode(x)[:300] for x in (search.findGfsDiagnosticObjects(gfs, query) or [])]
        except (Exception, Throwable) as e:
            out["diagnostic_objects_error"] = error_text(e)
    finally:
        if ref is not None:
            context.ungetService(ref)
    return out


def handle_gfs_explore(cmd):
    """Read-only: reports whether a diagnosis session is active and, if so, the current IGfs
    object with the methods available on it and on the GFS search service. Used to design the
    fault-guidance action; takes no connection to the vehicle."""
    out = {"ok": True}
    context = get_context()
    info, ref = gfs_service(context, "de.volkswagen.odis.vaudas.diag.systemtest.IDiagnosisSessionInfoService")
    try:
        if info is not None:
            out["diagnosis_active"] = info.isDiagnosisActive()
            out["diagnosis_started"] = info.isDiagnosisStarted()
            out["diagnosis_state"] = unicode(info.getDiagnosisState())
            out["vehicle_name"] = info.getVehicleName()
            out["brand"] = info.getBrand()
    except (Exception, Throwable) as e:
        out["session_error"] = error_text(e)
    finally:
        if ref is not None:
            context.ungetService(ref)
    try:
        gfs = current_gfs(context)
        out["gfs_class"] = gfs.getClass().getName() if gfs is not None else None
        out["gfs_methods"] = gfs_methods(gfs)
        if gfs is not None:
            try:
                plan = gfs.getInitialTestPlan()
                out["plan_class"] = plan.getClass().getName() if plan is not None else None
                out["plan_methods"] = gfs_methods(plan)
            except (Exception, Throwable) as e:
                out["plan_error"] = error_text(e)
            try:
                kb = gfs.getKnowledgeBase()
                out["kb_class"] = kb.getClass().getName() if kb is not None else None
                out["kb_methods"] = gfs_methods(kb)
            except (Exception, Throwable) as e:
                out["kb_error"] = error_text(e)
    except (Exception, Throwable) as e:
        out["gfs_error"] = error_text(e)
    search, ref3 = gfs_service(context, "de.volkswagen.odis.vaudas.gfs.search.IGfsSearchService")
    try:
        out["search_methods"] = gfs_methods(search)
    finally:
        if ref3 is not None:
            context.ungetService(ref3)
    return out


def handle_project_state(cmd):
    """Read-only: the current/available vehicle project, to design offline simulation."""
    from de.volkswagen.odis.vaudas.vehiclefunction.automation.IDiagnosticInterface import Factory
    out = {"ok": True}
    diag = Factory.getInstance()
    for name, fn in [("vehicle_project", diag.getVehicleProject),
                     ("change_allowed", diag.isVehicleProjectChangeAllowed),
                     ("project_mode", diag.getAutomationApiVersion)]:
        try:
            out[name] = fn()
        except (Exception, Throwable) as e:
            out[name + "_error"] = error_text(e)
    try:
        out["project_ecus"] = [unicode(x) for x in diag.getProjectEcuAddressList()]
    except (Exception, Throwable) as e:
        out["project_ecus_error"] = error_text(e)
    context = get_context()
    info, ref = gfs_service(context, "de.volkswagen.odis.vaudas.diag.systemtest.IDiagnosisSessionInfoService")
    try:
        if info is not None:
            out["proposed_manufacturers"] = [unicode(x) for x in info.getProposedManufacturerList()]
            out["proposed_projects"] = [unicode(x) for x in info.getProposedVehicleProjectList()]
    except (Exception, Throwable) as e:
        out["proposed_error"] = error_text(e)
    finally:
        if ref is not None:
            context.ungetService(ref)
    return out


def handle_api_methods(cmd):
    """Read-only: the methods of the ODIS automation diagnostic interface (IDiagnosticInterface),
    to see which read/write operations (coding, adaptation) are available over the bridge."""
    from de.volkswagen.odis.vaudas.vehiclefunction.automation.IDiagnosticInterface import Factory
    diag = Factory.getInstance()
    return {"ok": True, "class": diag.getClass().getName(), "methods": gfs_methods(diag)}


def handle_vehicle(cmd):
    """Just the current vehicle project, for a lightweight top-bar status poll."""
    from de.volkswagen.odis.vaudas.vehiclefunction.automation.IDiagnosticInterface import Factory
    try:
        return {"ok": True, "project": Factory.getInstance().getVehicleProject()}
    except (Exception, Throwable):
        return {"ok": True, "project": None}


def handle_vin(cmd):
    """Best-effort vehicle VIN from the current ODIS session; empty if ODIS does not know it."""
    out = {"ok": True, "vin": None}
    context = get_context()
    info, ref = gfs_service(context, "de.volkswagen.odis.vaudas.diag.systemtest.IDiagnosisSessionInfoService")
    try:
        for getter in ("getVehicleIdentificatonData", "getIdleVin"):
            try:
                obj = getattr(info, getter)()
            except (Exception, Throwable):
                continue
            if obj is None:
                continue
            for m in ("getVin", "getVIN", "getFin", "getVehicleIdentificationNumber", "getIdentNumber", "toString"):
                try:
                    v = getattr(obj, m)()
                except (Exception, Throwable):
                    continue
                if v is None:
                    continue
                mo = re.search(r"[A-HJ-NPR-Z0-9]{17}", unicode(v))  # a VIN is 17 chars, no I/O/Q
                if mo:
                    out["vin"] = mo.group(0)
                    return out
    finally:
        if ref is not None:
            context.ungetService(ref)
    return out


def handle_vci(cmd):
    """The vehicle communication interface (VCI) state/type, for the top-bar status."""
    from de.volkswagen.odis.vaudas.vehiclefunction.automation.IDiagnosticInterface import Factory
    diag = Factory.getInstance()
    out = {"ok": True}
    for k, fn in (("state", diag.checkVciState), ("clamp15", diag.getClamp15State), ("clamp30", diag.getClamp30State)):
        try:
            out[k] = unicode(fn())
        except (Exception, Throwable):
            pass
    try:
        out["doip"] = [unicode(x) for x in (diag.getDoIPVCIs() or [])]
    except (Exception, Throwable):
        pass
    return out


def handle_vehicle_projects(cmd):
    """The current vehicle project and whether it can be changed, plus any selectable projects
    ODIS proposes. Used to let the user pick the car for offline/guided data when not detected."""
    from de.volkswagen.odis.vaudas.vehiclefunction.automation.IDiagnosticInterface import Factory
    diag = Factory.getInstance()
    out = {"ok": True}
    for name, fn in [("current", diag.getVehicleProject), ("change_allowed", diag.isVehicleProjectChangeAllowed)]:
        try:
            out[name] = fn()
        except (Exception, Throwable) as e:
            out[name + "_error"] = error_text(e)
    for name, fn in [("vehicle_infos", lambda: diag.getVehicleInfos("")), ("odx_versions", diag.getODXVersions)]:
        try:
            out[name] = [unicode(x) for x in (fn() or [])]
        except (Exception, Throwable) as e:
            out[name + "_error"] = error_text(e)
    context = get_context()
    info, ref = gfs_service(context, "de.volkswagen.odis.vaudas.diag.systemtest.IDiagnosisSessionInfoService")
    try:
        if info is not None:
            out["proposed_projects"] = [unicode(x) for x in (info.getProposedVehicleProjectList() or [])]
            out["proposed_manufacturers"] = [unicode(x) for x in (info.getProposedManufacturerList() or [])]
    except (Exception, Throwable) as e:
        out["proposed_error"] = error_text(e)
    finally:
        if ref is not None:
            context.ungetService(ref)
    return out


def handle_set_vehicle_project(cmd):
    """Sets the vehicle project (the car) so its ODX data loads when ODIS did not detect one."""
    from de.volkswagen.odis.vaudas.vehiclefunction.automation.IDiagnosticInterface import Factory
    project = cmd.get("project")
    if not project:
        return {"ok": False, "error": "missing project"}
    diag = Factory.getInstance()
    try:
        result = diag.setVehicleProject(project)
    except (Exception, Throwable) as e:
        return {"ok": False, "error": error_text(e)}
    ecus = []
    try:
        ecus = [unicode(x) for x in (diag.getProjectEcuAddressList() or [])]
    except (Exception, Throwable):
        pass
    return {"ok": True, "project": unicode(result), "ecu_count": len(ecus)}


def handle_list_services(cmd):
    """Read-only: registered OSGi service class names containing cmd['q'] (for exploration)."""
    q = (cmd.get("q") or "").lower()
    context = get_context()
    names = set()
    for r in context.getAllServiceReferences(None, None) or []:
        for n in (r.getProperty("objectClass") or []):
            if q in n.lower():
                names.add(n)
    return {"ok": True, "services": sorted(names)}


def handle_service_methods(cmd):
    """Read-only: the methods of any registered OSGi service, for exploration."""
    context = get_context()
    svc, ref = gfs_service(context, cmd.get("service"))
    try:
        if svc is None:
            return {"ok": False, "error": "service not found"}
        return {"ok": True, "class": svc.getClass().getName(), "methods": gfs_methods(svc)}
    finally:
        if ref is not None:
            context.ungetService(ref)


HANDLERS = {
    "_debug": handle__debug,
    "gfs_explore": handle_gfs_explore,
    "fault_guidance": handle_fault_guidance,
    "service_methods": handle_service_methods,
    "list_services": handle_list_services,
    "vehicle": handle_vehicle,
    "vin": handle_vin,
    "vci": handle_vci,
    "vehicle_projects": handle_vehicle_projects,
    "set_vehicle_project": handle_set_vehicle_project,
    "api_methods": handle_api_methods,
    "project_state": handle_project_state,
    "ping": handle_ping,
    "list_project_ecus": handle_list_project_ecus,
    "list_vehicle_ecus": handle_list_vehicle_ecus,
    "find_ecu": handle_find_ecu,
    "get_ecu_coding_info": handle_get_ecu_coding_info,
    "create_coding_job": handle_create_coding_job,
    "get_coding_items": handle_get_coding_items,
    "get_coding_structure": handle_get_coding_structure,
    "read_coding_values": handle_read_coding_values,
    "read_identification": handle_read_identification,
    "read_coding": handle_read_coding,
    "list_coding_values": handle_list_coding_values,
    "list_adaptation_channels": handle_list_adaptation_channels,
    "read_adaptation": handle_read_adaptation,
    "read_adaptation_values": handle_read_adaptation_values,
    "adaptation_options": handle_adaptation_options,
    "write_byte_coding": handle_write_byte_coding,
    "write_adaptation": handle_write_adaptation,
    "list_access_modes": handle_list_access_modes,
    "login": handle_login,
    "list_measurement_groups": handle_list_measurement_groups,
    "list_measurements": handle_list_measurements,
    "read_measurement": handle_read_measurement,
    "read_measurement_group": handle_read_measurement_group,
    "read_gateway_component_list": handle_read_gateway_component_list,
    "read_event_memory": handle_read_event_memory,
    "clear_event_memory": handle_clear_event_memory,
    "odx_variants": handle_odx_variants,
    "odx_unit": handle_odx_unit,
    "odx_dtcs": handle_odx_dtcs,
    "odx_table": handle_odx_table,
    "odx_textual_variants": handle_odx_textual_variants,
}
