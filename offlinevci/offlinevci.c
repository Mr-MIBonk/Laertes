// Laertes offline VCI: a SAE J2534-1 (04.04) pass-through device without hardware, with a small
// CAN gateway simulator.
//
// ODIS enters a vehicle project only when a VCI is connected. This DLL is registered as a J2534
// device (see offlinevci.go): it opens and connects and reports 12 V battery voltage, so ODIS loads
// the project and runs the bridge macro with no interface attached.
//
// On every ISO15765 (CAN/ISO-TP) channel it also answers diagnostic requests as a control unit
// would. For raw CAN (protocol 5), it performs ISO-TP framing/reassembly itself. It serves the
// identification handshake a VW gateway answers -- DiagnosticSessionControl (0x10), TesterPresent
// (0x3E) and ReadDataByIdentifier (0x22) -- with canned values. WriteDataByIdentifier (0x2E) accepts
// any DID (coding 0x0600, workshop code 0xF198, programming date 0xF199, ...) and reads it back;
// SecurityAccess (0x27) uses a zero seed.
//
// Pass-through: when offline.conf names a real J2534 driver (passthru=), every call is forwarded to
// it instead, so ODIS works with the real interface through this DLL.
//
// Wake-up: with wake=1, once ODIS connects a channel a thread sends one CAN frame periodically
// (by default VW Klemmen_Status_01 0x3C0 with clamp 15 on, with its E2E counter and CRC), so units
// on a bench wake up and act as if the ignition were on. Through a real driver it goes on the bus;
// on the simulated device it only appears in the trace (kind W). It is sent only while the file
// wake.on exists next to this DLL: Laertes's Ignition button creates and deletes it.
//
// Build (32-bit): offlinevci\build.cmd

#include <windows.h>
#include <stddef.h>
#include <string.h>
#include <stdlib.h>

#define STATUS_NOERROR          0x00
#define ERR_NULL_PARAMETER      0x04
#define ERR_DEVICE_NOT_CONNECTED 0x08
#define ERR_TIMEOUT             0x09
#define ERR_BUFFER_EMPTY        0x10

#define CAN      0x05
#define ISO15765 0x06

#define GET_CONFIG        0x01
#define SET_CONFIG        0x02
#define READ_VBATT        0x03
#define READ_PROG_VOLTAGE 0x04

#define LOOPBACK    0x03
#define TX_MSG_TYPE 0x01

#define CAN_29BIT_ID 0x100
#define CAN_ID_BOTH  0x800

typedef struct {
    unsigned long Parameter;
    unsigned long Value;
} SCONFIG;

typedef struct {
    unsigned long NumOfParams;
    SCONFIG *ConfigPtr;
} SCONFIG_LIST;

typedef struct {
    unsigned long ProtocolID;
    unsigned long RxStatus;
    unsigned long TxFlags;
    unsigned long Timestamp;
    unsigned long DataSize;
    unsigned long ExtraDataIndex;
    unsigned char Data[4128];
} PASSTHRU_MSG;

static volatile LONG nextId = 0;

static unsigned long newId(unsigned long *id) {
    if (!id) return ERR_NULL_PARAMETER;
    *id = (unsigned long)InterlockedIncrement(&nextId);
    return STATUS_NOERROR;
}

// --- Simulation state ----------------------------------------------------------------------------

#define MAXCH     32
#define MAXF      8
#define MAXQ      64
#define MAXRESP   512
#define MAXREQ    256
#define MAXWRITE  64 // bytes per written DID value
#define MAXWRITTEN 32 // DIDs that can hold a written value

typedef struct {
    int rxActive;
    unsigned long rxId;
    unsigned long rxLen, rxExp;
    unsigned char rxBuf[MAXREQ];

    int txActive;
    unsigned long txRespId;
    unsigned long txLen, txOff;
    unsigned char txSeq;
    unsigned char txBuf[MAXRESP];
} IsoTp;

typedef struct {
    unsigned long tx; // tester request CAN ID
    unsigned long rx; // ECU response CAN ID
} Filter;

typedef struct {
    unsigned long id;
    unsigned long proto;
    unsigned long nflt;
    Filter flt[MAXF];
    IsoTp tp;
    int loopback;
} Channel;

typedef struct {
    unsigned long ch;
    unsigned long proto;
    unsigned long canId;
    unsigned long len;
    unsigned char data[MAXRESP];
    unsigned long ts;
    unsigned long long due;
    unsigned long rxStatus;
} Response;

static SRWLOCK lock = SRWLOCK_INIT;
static CONDITION_VARIABLE queued = CONDITION_VARIABLE_INIT;

static SRWLOCK logLock = SRWLOCK_INIT;

static unsigned long long usNow(void) {
    static LARGE_INTEGER freq;
    LARGE_INTEGER t;
    if (!freq.QuadPart) QueryPerformanceFrequency(&freq);
    QueryPerformanceCounter(&t);
    return (unsigned long long)t.QuadPart * 1000000ULL / (unsigned long long)freq.QuadPart;
}

static Channel channels[MAXCH];
static Response queue[MAXQ];
static unsigned long qHead, qCount;

// Gateway addressing and response latency are configurable.
static unsigned long gwTx = 0x710;
static unsigned long gwRx = 0x77A;
static unsigned long long respDelayUs = 0; // 0 = ODIS can read the answer immediately

static Channel *findChannel(unsigned long id) {
    for (int i = 0; i < MAXCH; i++)
        if (channels[i].id == id) return &channels[i];
    return 0;
}

static unsigned long canId4(const unsigned char *d) {
    return ((unsigned long)d[0] << 24) | ((unsigned long)d[1] << 16) |
           ((unsigned long)d[2] << 8) | (unsigned long)d[3];
}

// --- Gateway identification data -----------------------------------------------------------------

typedef struct {
    unsigned short did;
    const char *value;
} Did;

// Values are illustrative MQB gateway (J533) data. ODIS validates the format against the ODX
// description, so individual fields can be refined later; the important part is that every DID
// returns data instead of NRC 0x31.
static const Did dids[] = {
    // Standardized UDS identification DIDs (ISO 14229, 0xF180..0xF19F)
    {0xF180, "5Q0907530J"},              // boot software identification
    {0xF181, "0356"},                    // application software identification
    {0xF182, "0356"},                    // application data identification
    {0xF183, "000000000000001"},         // boot software fingerprint
    {0xF184, "000000000000001"},         // application software fingerprint
    {0xF185, "000000000000001"},         // application data fingerprint
    {0xF186, "\x01"},                    // active diagnostic session
    {0xF187, "5Q0907530J"},              // VW/Audi part number
    {0xF188, "5Q0907530J"},              // software number
    {0xF189, "0356"},                    // software version
    {0xF18A, "VW"},                      // system supplier identifier
    {0xF18B, "01012020"},                // ECU manufacturing date
    {0xF18C, "000000000000001"},         // ECU serial number
    {0xF18D, "\x01\x02\x03\x04"},        // supported functional units
    {0xF18E, "5Q0907530J"},              // kit assembly part number
    {0xF192, "H03"},                     // system supplier hardware number
    {0xF193, "H03"},                     // hardware version
    {0xF194, "0356"},                    // system supplier software number
    {0xF195, "0356"},                    // system supplier software version
    {0xF196, "EU"},                      // type approval
    {0xF197, "GW MQB High"},             // system name / engine type
    {0xF198, "000000000000001"},         // repair shop code / tester serial
    {0xF199, "01012020"},                // programming date
    {0xF19A, "000000000000001"},         // calibration workshop code
    {0xF19B, "01012020"},                // calibration date
    {0xF19C, "0356"},                    // calibration equipment software
    {0xF19D, "01012020"},                // ECU installation date
    {0xF19E, "GW_MQB_High"},             // ASAM/ODX file name
    {0xF19F, "0001"},                    // ODX file version / entity

    // VW-specific gateway identification (illustrative; refine from vci-can.log later)
    {0x0600, ""},                        // coding: in written[]
    {0x0602, "\x00\x00\x00\x00"},        // vehicle equipment code
    {0x0603, "\x00\x00\x00\x00"},        // parameter set data
    {0x0610, "GW"},                      // system abbreviation
    {0x0611, "GW MQB High"},             // system designation
    {0x0612, "\x00\x00\x00\x00"},        // bus master / sub-bus master
    {0x0613, "\x00\x00\x00\x00"},        // connected components
    {0x0614, "\x00\x00\x00\x00"},        // detected subsystems
    {0x0615, "\x00\x00\x00\x00"},        // FAZIT identification
    {0x0616, "\x00\x00\x00\x00"},        // engine code
    {0x0617, "\x00"},                    // PVD level
    {0x0618, "\x00\x00\x00\x00"},        // software module version
    {0x0619, "\x00\x00\x00\x00"},        // lastenheft version
    {0x061A, "\x00\x00\x00\x00"},        // data record identification
    {0x061B, "\x00\x00\x00\x00"},        // configuration integrity data
    {0x061C, "\x00\x00\x00\x00"},        // programming integrity data
    {0x061D, "\x00\x00\x00\x00"},        // EEPROM consistency
    {0x061E, "\x00\x00\x00\x00"},        // flash data security
    {0x061F, "\x00\x00\x00\x00"},        // diagnostic mode
};

// VIN (DID 0xF190) is configurable via offline.conf and handled separately below.
static char vin[18] = "WVWZZZ1KZAW000001"; // 17 chars + NUL

static const Did *findDid(unsigned short did) {
    for (unsigned i = 0; i < sizeof(dids) / sizeof(dids[0]); i++)
        if (dids[i].did == did) return &dids[i];
    return 0;
}

// Values written with WriteDataByIdentifier (coding 0x0600, workshop code 0xF198, programming
// date 0xF199, ...). A written value overrides the table and is read back until the DLL unloads.
typedef struct {
    unsigned short did;
    unsigned long len;
    unsigned char data[MAXWRITE];
} Written;

static Written written[MAXWRITTEN] = {
    {0x0600, 4, {0x03, 0x3F, 0x01, 0x00}}, // coding
    // State of Immobilizer (cluster DID 0x02ED, Param_State): a 10-byte raw field. Component
    // protection (J285 KS) goes on only with byte 0 = 0x02, found by trying 00-1F: 00 and 01 stop
    // it with "Der Wegfahrsperren-Master hat den Zustand 01 … Passen Sie zuerst die Wegfahrsperre an".
    {0x02ED, 10, {0x02, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
    // State of Immobilizer Slaves (cluster DID 0x02EE, Param_KeyState, Param_ImmobState): key state
    // 0x84, found by trying. Without bit 0x04 ODIS asks for a valid key and the ignition on; with it
    // but without 0x80, "Der Schlüssel ist nicht authentifizierbar".
    {0x02EE, 10, {0x84, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00}},
    // Transponder ID of the current key (cluster DID 0x02E4, Param_KeyTransID): a non-zero value so
    // component protection sees a key at the reading coil and does not wait for one to be presented.
    {0x02E4, 4, {0x12, 0x34, 0x56, 0x78}},
};
static unsigned writtenCount = 4;

static Written *findWritten(unsigned short did) {
    for (unsigned i = 0; i < writtenCount; i++)
        if (written[i].did == did) return &written[i];
    return 0;
}

// persistDid (defined below) records a written value in dids.txt so it survives a DLL reload.
static void persistDid(unsigned short did, const unsigned char *data, unsigned long len);

// Stores a written value; returns 0 or a negative response code.
static unsigned char writeDid(unsigned short did, const unsigned char *data, unsigned long len) {
    if (len == 0 || len > MAXWRITE) return 0x13; // incorrectMessageLengthOrInvalidFormat

    Written *w = findWritten(did);
    if (!w) {
        if (writtenCount >= MAXWRITTEN) return 0x22; // conditionsNotCorrect
        w = &written[writtenCount++];
        w->did = did;
    }

    memcpy(w->data, data, len);
    w->len = len;
    persistDid(did, data, len); // keep the value across a DLL reload (read back by didOverride)
    return 0;
}

// Generic fallback for DIDs that are not in the table. Returning a short neutral value prevents
// ODIS from aborting the whole identification with NRC 0x31; ODIS may show such a field as
// "Chybný výsledek" until its exact ODX format/value is provided.
static unsigned char genericDidValue[8] = {0};

static void laertesFile(char *out, unsigned long cap, const char *name);
static void logStr(const char *s);

// dids.txt next to the configuration: lines "02ED 01 00 00 ..." (DID, then the value bytes in hex)
// that the simulated units answer instead of their own values. Read on every request, so a value
// can be changed while ODIS runs (e.g. to try which immobilizer state a guided function accepts).
static unsigned char overrideValue[MAXWRITE];

static int didOverride(unsigned short did, unsigned long *len) {
    char path[MAX_PATH];
    laertesFile(path, sizeof path, "dids.txt");
    if (!path[0]) return 0;
    HANDLE f = CreateFileA(path, GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE, 0,
                           OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, 0);
    if (f == INVALID_HANDLE_VALUE) return 0;
    char buf[4096];
    DWORD got = 0;
    BOOL ok = ReadFile(f, buf, sizeof buf - 1, &got, 0);
    CloseHandle(f);
    if (!ok) return 0;
    buf[got] = 0;

    for (char *p = buf; *p;) {
        char *end;
        unsigned long id = strtoul(p, &end, 16);
        if (end != p && id == did) {
            unsigned long n = 0;
            for (p = end; n < MAXWRITE;) {
                while (*p == ' ' || *p == '\t') p++;
                if (!*p || *p == '\r' || *p == '\n') break; // strtoul would go on to the next line
                unsigned long b = strtoul(p, &end, 16);
                if (end == p || b > 0xFF) break;
                overrideValue[n++] = (unsigned char)b;
                p = end;
            }
            if (n == 0) return 0;
            *len = n;
            return 1;
        }
        while (*p && *p != '\n') p++;
        if (*p) p++;
    }
    return 0;
}

// persistDid records a written DID value in dids.txt so it survives a DLL reload: written[] is only
// in-memory, but didOverride reads dids.txt back at the highest precedence, so the next read -- this
// session or after a restart -- returns the written value. Every other line (comments, other DIDs) is
// preserved; only this DID's line is rewritten, appended at the end. The file is swapped in atomically
// via a temp file so a concurrent didOverride read never sees it half-written. A file larger than the
// buffer is left untouched rather than risk truncating it.
static SRWLOCK didFileLock = SRWLOCK_INIT;

static void persistDid(unsigned short did, const unsigned char *data, unsigned long len) {
    char path[MAX_PATH], tmp[MAX_PATH];
    laertesFile(path, sizeof path, "dids.txt");
    laertesFile(tmp, sizeof tmp, "dids.txt.new");
    if (!path[0] || !tmp[0]) return;

    AcquireSRWLockExclusive(&didFileLock);

    static char in[16384];
    DWORD got = 0;
    HANDLE f = CreateFileA(path, GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE, 0,
                           OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, 0);
    if (f != INVALID_HANDLE_VALUE) {
        ReadFile(f, in, sizeof in - 1, &got, 0);
        CloseHandle(f);
    }
    if (got >= sizeof in - 1) { // file too large to rewrite safely
        ReleaseSRWLockExclusive(&didFileLock);
        return;
    }
    in[got] = 0;

    static char out[sizeof in + 256];
    unsigned long o = 0;

    // Copy every existing line except the one that defines this DID.
    for (char *p = in; *p;) {
        char *line = p;
        while (*p && *p != '\n') p++;
        if (*p) p++; // step past '\n'
        unsigned long full = (unsigned long)(p - line); // line incl. its newline, if any

        char *end;
        unsigned long id = strtoul(line, &end, 16);
        if (end != line && id == did) continue; // drop the old value for this DID

        if (o + full < sizeof out) {
            memcpy(out + o, line, full);
            o += full;
        }
    }

    if (o > 0 && out[o - 1] != '\n' && o < sizeof out) out[o++] = '\n';

    // Append "<DID> <byte> <byte> ..." for the new value, in the format didOverride parses.
    static const char hex[] = "0123456789ABCDEF";
    if (o + 5 + len * 3 + 1 < sizeof out) {
        out[o++] = hex[(did >> 12) & 0xF];
        out[o++] = hex[(did >> 8) & 0xF];
        out[o++] = hex[(did >> 4) & 0xF];
        out[o++] = hex[did & 0xF];
        for (unsigned long i = 0; i < len; i++) {
            out[o++] = ' ';
            out[o++] = hex[(data[i] >> 4) & 0xF];
            out[o++] = hex[data[i] & 0xF];
        }
        out[o++] = '\n';
    }

    HANDLE wf = CreateFileA(tmp, GENERIC_WRITE, 0, 0, CREATE_ALWAYS, FILE_ATTRIBUTE_NORMAL, 0);
    if (wf != INVALID_HANDLE_VALUE) {
        DWORD w = 0;
        WriteFile(wf, out, o, &w, 0);
        CloseHandle(wf);
        MoveFileExA(tmp, path, MOVEFILE_REPLACE_EXISTING);
    }

    ReleaseSRWLockExclusive(&didFileLock);
}

static const unsigned char *didValue(unsigned short did, unsigned long *len) {
    if (didOverride(did, len)) {
        logStr("DID value from dids.txt");
        return overrideValue;
    }

    if (did == 0xF190) {
        *len = (unsigned long)strlen(vin);
        return (const unsigned char *)vin;
    }

    const Written *w = findWritten(did);
    if (w) {
        *len = w->len;
        return w->data;
    }

    const Did *d = findDid(did);
    if (d) {
        *len = (unsigned long)strlen(d->value);
        return (const unsigned char *)d->value;
    }

    // Unknown DID: return a neutral 8-byte value instead of NRC 0x31.
    *len = sizeof(genericDidValue);
    return genericDidValue;
}

// --- Files and configuration ---------------------------------------------------------------------

static HMODULE selfModule;

static void laertesFile(char *out, unsigned long cap, const char *name) {
    char pd[MAX_PATH];
    unsigned long n = GetEnvironmentVariableA("ProgramData", pd, MAX_PATH);
    if (n == 0 || n >= MAX_PATH) lstrcpynA(pd, "C:\\ProgramData", MAX_PATH);

    if (lstrlenA(pd) + (int)sizeof("\\OE\\laertes\\") + lstrlenA(name) + 1 > (int)cap) {
        out[0] = 0;
        return;
    }

    lstrcpynA(out, pd, cap);
    lstrcatA(out, "\\OE\\laertes\\");
    lstrcatA(out, name);
}

// --- CAN trace -----------------------------------------------------------------------------------

#define CANLOG_CAP (2 * 1024 * 1024)
#define CANLOG_MAXBYTES 64

static char canlogScratch[CANLOG_CAP / 2];

static char hexDigit(unsigned v) { return "0123456789ABCDEF"[v & 0xF]; }

static void putHexU32(char *buf, unsigned long *o, unsigned long v, int minDigits) {
    char tmp[8];
    int k = 0;
    do { tmp[k++] = hexDigit((unsigned)(v & 0xF)); v >>= 4; } while (v);
    while (k < minDigits) tmp[k++] = '0';
    while (k) buf[(*o)++] = tmp[--k];
}

static void logWrite(const char *body, unsigned long n) {
    char path[MAX_PATH];
    laertesFile(path, sizeof path, "vci-can.log");
    if (!path[0]) return;

    AcquireSRWLockExclusive(&logLock);

    HANDLE f = CreateFileA(path, GENERIC_READ | GENERIC_WRITE,
                           FILE_SHARE_READ | FILE_SHARE_WRITE, 0,
                           OPEN_ALWAYS, FILE_ATTRIBUTE_NORMAL, 0);
    if (f == INVALID_HANDLE_VALUE) {
        ReleaseSRWLockExclusive(&logLock);
        return;
    }

    DWORD sz = GetFileSize(f, 0);
    if (sz != INVALID_FILE_SIZE && sz > CANLOG_CAP) {
        DWORD keep = CANLOG_CAP / 2, got = 0, start = 0;
        SetFilePointer(f, (LONG)(sz - keep), 0, FILE_BEGIN);
        if (ReadFile(f, canlogScratch, keep, &got, 0)) {
            while (start < got && canlogScratch[start] != '\n') start++;
            if (start < got) start++;
        } else {
            got = start = 0;
        }

        SetFilePointer(f, 0, 0, FILE_BEGIN);
        SetEndOfFile(f);
        if (got > start) {
            DWORD w;
            WriteFile(f, canlogScratch + start, got - start, &w, 0);
        }
    }

    SetFilePointer(f, 0, 0, FILE_END);

    char buf[48 + 2 * CANLOG_MAXBYTES];
    unsigned long o = 0;
    unsigned long t = GetTickCount();
    char num[12];
    int ni = 0;

    if (!t) num[ni++] = '0';
    while (t) { num[ni++] = (char)('0' + t % 10); t /= 10; }
    while (ni) buf[o++] = num[--ni];

    buf[o++] = ' ';
    for (unsigned long i = 0; i < n && o < sizeof buf - 1; i++) buf[o++] = body[i];
    buf[o++] = '\n';

    DWORD wrote;
    WriteFile(f, buf, o, &wrote, 0);
    CloseHandle(f);

    ReleaseSRWLockExclusive(&logLock);
}

// logStr writes a plain text marker line to the CAN trace.
static void logStr(const char *s) {
    unsigned long n = 0;
    while (s[n]) n++;
    logWrite(s, n);
}

// Trace format: "<ms> R <ch> <canId> <hex>" / "<ms> T <ch> <canId> <hex>"
static void canLog(char dir, unsigned long ch, unsigned long canId,
                   const unsigned char *data, unsigned long len) {
    char b[16 + 2 * CANLOG_MAXBYTES];
    unsigned long o = 0;

    b[o++] = dir;
    b[o++] = ' ';
    putHexU32(b, &o, ch, 1);
    b[o++] = ' ';
    putHexU32(b, &o, canId, 3);
    b[o++] = ' ';

    if (len > CANLOG_MAXBYTES) len = CANLOG_MAXBYTES;
    for (unsigned long i = 0; i < len; i++) {
        b[o++] = hexDigit(data[i] >> 4);
        b[o++] = hexDigit(data[i]);
    }

    logWrite(b, o);
}

static void canLogFilter(unsigned long ch, unsigned long type,
                         unsigned long mask, unsigned long pattern, unsigned long flowControl) {
    char b[96];
    unsigned long o = 0;

    b[o++] = 'F'; b[o++] = 'I'; b[o++] = 'L'; b[o++] = 'T'; b[o++] = ' ';
    putHexU32(b, &o, ch, 1);
    b[o++] = ' ';
    putHexU32(b, &o, type, 1);
    b[o++] = ' ';
    putHexU32(b, &o, mask, 3);
    b[o++] = ' ';
    putHexU32(b, &o, pattern, 3);
    b[o++] = ' ';
    putHexU32(b, &o, flowControl, 3);

    logWrite(b, o);
}

static void confPath(char *out, unsigned long cap) { laertesFile(out, cap, "offline.conf"); }

// --- Pass-through and wake-up configuration (see the top of this file) ---------------------------

static char passthruPath[MAX_PATH];
static int wakeOn;
static unsigned long wakeId = 0x3C0;
static unsigned char wakeData[8] = {0x00, 0x00, 0x03, 0x00}; // clamp S and 15 on
static unsigned long wakeLen = 4;
static int wakeE2E = 0xC3; // E2E data ID of Klemmen_Status_01; -1 sends wakeData unchanged
static unsigned long wakeMs = 100;

static int hexVal(char c) {
    if (c >= '0' && c <= '9') return c - '0';
    if (c >= 'a' && c <= 'f') return c - 'a' + 10;
    if (c >= 'A' && c <= 'F') return c - 'A' + 10;
    return -1;
}

static void applyConfig(const char *key, const char *value) {
    if (lstrcmpA(key, "passthru") == 0) {
        lstrcpynA(passthruPath, value, MAX_PATH);
        return;
    }

    if (lstrcmpA(key, "wake") == 0) {
        wakeOn = value[0] == '1';
        return;
    }

    if (lstrcmpA(key, "wake_id") == 0 && value[0]) {
        wakeId = strtoul(value, 0, 16);
        return;
    }

    if (lstrcmpA(key, "wake_data") == 0 && value[0]) {
        unsigned long n = 0;
        for (const char *p = value; p[0] && p[1] && n < 8; p += 2) {
            int hi = hexVal(p[0]), lo = hexVal(p[1]);
            if (hi < 0 || lo < 0) break;
            wakeData[n++] = (unsigned char)(hi << 4 | lo);
        }
        if (n) wakeLen = n;
        return;
    }

    if (lstrcmpA(key, "wake_e2e") == 0) {
        wakeE2E = value[0] ? (int)strtoul(value, 0, 16) : -1;
        return;
    }

    if (lstrcmpA(key, "wake_ms") == 0 && value[0]) {
        wakeMs = strtoul(value, 0, 10);
        if (wakeMs < 10) wakeMs = 10;
        return;
    }

    if (lstrcmpA(key, "vin") == 0 && value[0]) {
        unsigned long i = 0;
        while (value[i] && i < sizeof(vin) - 1) { vin[i] = value[i]; i++; }
        vin[i] = 0;
        return;
    }

    if (lstrcmpA(key, "gw_tx") == 0 && value[0]) {
        gwTx = strtoul(value, 0, 0);
        return;
    }

    if (lstrcmpA(key, "gw_rx") == 0 && value[0]) {
        gwRx = strtoul(value, 0, 0);
        return;
    }

    if (lstrcmpA(key, "resp_delay_us") == 0 && value[0]) {
        respDelayUs = strtoull(value, 0, 0);
        return;
    }
}

static void loadConfig(void) {
    char path[MAX_PATH];
    confPath(path, sizeof path);
    if (!path[0]) return;

    HANDLE f = CreateFileA(path, GENERIC_READ, FILE_SHARE_READ | FILE_SHARE_WRITE, 0,
                           OPEN_EXISTING, FILE_ATTRIBUTE_NORMAL, 0);
    if (f == INVALID_HANDLE_VALUE) return;

    char buf[4096];
    DWORD got = 0;
    BOOL ok = ReadFile(f, buf, sizeof buf - 1, &got, 0);
    CloseHandle(f);
    if (!ok) return;
    buf[got] = 0;

    AcquireSRWLockExclusive(&lock);

    passthruPath[0] = 0; // a session without these lines uses the simulated device and no wake-up
    wakeOn = 0;

    char *line = buf;
    while (line && *line) {
        char *nl = line;
        while (*nl && *nl != '\n') nl++;
        char stop = *nl;
        *nl = 0;

        char *endp = nl;
        while (endp > line && (endp[-1] == '\r' || endp[-1] == ' ' || endp[-1] == '\t')) *--endp = 0;

        char *eq = line;
        while (*eq && *eq != '=') eq++;
        if (*eq == '=') {
            *eq = 0;
            applyConfig(line, eq + 1);
        }

        line = stop ? nl + 1 : 0;
    }

    ReleaseSRWLockExclusive(&lock);
}

// --- UDS response generation ---------------------------------------------------------------------

static unsigned long negative(unsigned char sid, unsigned char nrc, unsigned char *out) {
    out[0] = 0x7F;
    out[1] = sid;
    out[2] = nrc;
    return 3;
}

static unsigned long buildResponse(const unsigned char *req, unsigned long n, unsigned char *out) {
    if (n == 0) return 0;

    unsigned char sid = req[0];
    int suppress = n > 1 && (req[1] & 0x80);

    switch (sid) {
    case 0x10: // DiagnosticSessionControl
        if (suppress) return 0;
        out[0] = 0x50;
        out[1] = n > 1 ? (unsigned char)(req[1] & 0x7F) : 0x01;
        out[2] = 0x00;
        out[3] = 0x32;
        out[4] = 0x01;
        out[5] = 0xF4;
        return 6;

    case 0x3E: // TesterPresent
        if (suppress) return 0;
        out[0] = 0x7E;
        out[1] = n > 1 ? (unsigned char)(req[1] & 0x7F) : 0x00;
        return 2;

    case 0x28: // CommunicationControl (e.g. the gateway's 28 C0 01 during component protection)
        if (n < 3)
            return negative(sid, 0x13, out);
        if (suppress) return 0;
        out[0] = 0x68;
        out[1] = (unsigned char)(req[1] & 0x7F);
        return 2;

    case 0x22: { // ReadDataByIdentifier
        if (n < 3 || ((n - 1) % 2) != 0)
            return negative(sid, 0x13, out);

        unsigned long o = 1;
        out[0] = 0x62;

        for (unsigned long i = 1; i + 1 < n; i += 2) {
            unsigned short did = (unsigned short)((req[i] << 8) | req[i + 1]);
            unsigned long vlen;
            const unsigned char *val = didValue(did, &vlen);
            if (!val) continue;

            if (o + 2 + vlen > MAXRESP) break;

            out[o++] = req[i];
            out[o++] = req[i + 1];
            memcpy(out + o, val, vlen);
            o += vlen;
        }

        if (o == 1)
            return negative(sid, 0x31, out);

        return o;
    }

    case 0x2E: { // WriteDataByIdentifier
        if (n < 4)
            return negative(sid, 0x13, out);

        unsigned short did = (unsigned short)((req[1] << 8) | req[2]);
        unsigned char nrc = writeDid(did, req + 3, n - 3);
        if (nrc)
            return negative(sid, nrc, out);

        out[0] = 0x6E;
        out[1] = req[1];
        out[2] = req[2];
        return 3;
    }

    case 0x27: { // SecurityAccess
        if (n < 2)
            return negative(sid, 0x13, out);

        out[0] = 0x67;
        out[1] = req[1];

        if (req[1] & 1) {
            out[2] = out[3] = out[4] = out[5] = 0x00;
            return 6;
        }

        return 2;
    }

    default:
        return negative(sid, 0x11, out);
    }
}

// --- Response queue ------------------------------------------------------------------------------

static void enqueueAt(const Channel *c, unsigned long canId, const unsigned char *data,
                      unsigned long len, unsigned long rxStatus, unsigned long long delayUs) {
    if (len == 0 || len > MAXRESP) return;

    if (qCount >= MAXQ) {
        logStr("QFULL drop");
        return;
    }

    Response *r = &queue[(qHead + qCount) % MAXQ];
    r->ch = c->id;
    r->proto = c->proto;
    r->canId = canId;
    r->len = len;
    memcpy(r->data, data, len);
    r->due = usNow() + delayUs;
    r->ts = (unsigned long)r->due;
    r->rxStatus = rxStatus;
    qCount++;

    WakeAllConditionVariable(&queued);
}

static void enqueue(const Channel *c, unsigned long canId, const unsigned char *data, unsigned long len) {
    enqueueAt(c, canId, data, len, 0, respDelayUs);
}

// --- Raw CAN with ISO-TP -------------------------------------------------------------------------

static unsigned long respIdFor(unsigned long reqId) {
    if (reqId == 0x7DF || reqId == gwTx) return gwRx;
    if (reqId >= 0x700 && reqId <= 0x73F) return reqId + 0x6A;
    return 0;
}

static void enqueueFrame(Channel *c, unsigned long canId, const unsigned char *f, unsigned long n) {
    unsigned char frame[8];
    memset(frame, 0x55, 8);
    if (n > 8) n = 8;
    memcpy(frame, f, n);
    enqueue(c, canId, frame, 8);
}

static void sendConsecutiveFrames(Channel *c) {
    while (c->tp.txOff < c->tp.txLen) {
        unsigned char f[8];
        f[0] = (unsigned char)(0x20 | (c->tp.txSeq & 0x0F));

        unsigned long n = c->tp.txLen - c->tp.txOff;
        if (n > 7) n = 7;

        memcpy(f + 1, c->tp.txBuf + c->tp.txOff, n);
        enqueueFrame(c, c->tp.txRespId, f, 1 + n);

        c->tp.txOff += n;
        c->tp.txSeq++;
    }

    c->tp.txActive = 0;
}

static void sendIsoTp(Channel *c, unsigned long respId, const unsigned char *data, unsigned long len) {
    if (len == 0 || len > MAXRESP) return;

    if (len <= 7) {
        unsigned char f[8];
        f[0] = (unsigned char)len;
        memcpy(f + 1, data, len);
        enqueueFrame(c, respId, f, 1 + len);
        return;
    }

    unsigned char f[8];
    f[0] = (unsigned char)(0x10 | ((len >> 8) & 0x0F));
    f[1] = (unsigned char)(len & 0xFF);
    memcpy(f + 2, data, 6);
    enqueueFrame(c, respId, f, 8);

    c->tp.txActive = 1;
    c->tp.txRespId = respId;
    c->tp.txLen = len;
    c->tp.txOff = 6;
    c->tp.txSeq = 1;
    memcpy(c->tp.txBuf, data, len);
}

static void answer(Channel *c, unsigned long respId, const unsigned char *uds, unsigned long n) {
    unsigned char out[MAXRESP];
    unsigned long len = buildResponse(uds, n, out);

    if (len) {
        canLog('T', c->id, respId, out, len);
        sendIsoTp(c, respId, out, len);
    }
}

static void handleCanFrame(Channel *c, unsigned long reqId, const unsigned char *p, unsigned long n) {
    if (n < 1) return;

    unsigned long respId = respIdFor(reqId);

    switch (p[0] & 0xF0) {
    case 0x00: { // single frame
        unsigned long len = p[0] & 0x0F;
        if (respId == 0 || len == 0 || len + 1 > n) return;
        answer(c, respId, p + 1, len);
        return;
    }

    case 0x10: { // first frame
        if (respId == 0 || n < 2) return;

        unsigned long len = ((unsigned long)(p[0] & 0x0F) << 8) | p[1];
        if (len > MAXREQ) len = MAXREQ;

        unsigned long take = n - 2 < 6 ? n - 2 : 6;
        if (take > len) take = len;

        memcpy(c->tp.rxBuf, p + 2, take);
        c->tp.rxActive = 1;
        c->tp.rxId = reqId;
        c->tp.rxLen = take;
        c->tp.rxExp = len;

        unsigned char fc[3] = {0x30, 0x00, 0x00};
        enqueueFrame(c, respId, fc, 3);
        return;
    }

    case 0x20: { // consecutive frame
        if (!c->tp.rxActive || reqId != c->tp.rxId) return;

        unsigned long take = c->tp.rxExp - c->tp.rxLen;
        if (take > 7) take = 7;
        if (take > n - 1) take = n - 1;

        memcpy(c->tp.rxBuf + c->tp.rxLen, p + 1, take);
        c->tp.rxLen += take;

        if (c->tp.rxLen >= c->tp.rxExp) {
            c->tp.rxActive = 0;
            answer(c, respId, c->tp.rxBuf, c->tp.rxExp);
        }
        return;
    }

    case 0x30:
        if (c->tp.txActive && (p[0] & 0x0F) == 0) sendConsecutiveFrames(c);
        return;
    }
}

// --- Pass-through to a real J2534 driver ---------------------------------------------------------

typedef long(WINAPI *OpenFn)(void *, unsigned long *);
typedef long(WINAPI *CloseFn)(unsigned long);
typedef long(WINAPI *ConnectFn)(unsigned long, unsigned long, unsigned long, unsigned long, unsigned long *);
typedef long(WINAPI *DisconnectFn)(unsigned long);
typedef long(WINAPI *MsgsFn)(unsigned long, void *, unsigned long *, unsigned long);
typedef long(WINAPI *StartPeriodicFn)(unsigned long, void *, unsigned long *, unsigned long);
typedef long(WINAPI *StopFn)(unsigned long, unsigned long);
typedef long(WINAPI *StartFilterFn)(unsigned long, unsigned long, void *, void *, void *, unsigned long *);
typedef long(WINAPI *VoltageFn)(unsigned long, unsigned long, unsigned long);
typedef long(WINAPI *VersionFn)(unsigned long, char *, char *, char *);
typedef long(WINAPI *LastErrorFn)(char *);
typedef long(WINAPI *IoctlFn)(unsigned long, unsigned long, void *, void *);

static struct {
    HMODULE lib;
    OpenFn open;
    CloseFn close;
    ConnectFn connect;
    DisconnectFn disconnect;
    MsgsFn read, write;
    StartPeriodicFn startPeriodic;
    StopFn stopPeriodic, stopFilter;
    StartFilterFn startFilter;
    VoltageFn voltage;
    VersionFn version;
    LastErrorFn lastError;
    IoctlFn ioctl;
} real;

// loadReal loads the driver named by passthru= once; it reports whether every entry point is there.
static int loadReal(void) {
    if (real.lib) return 1;
    HMODULE lib = LoadLibraryA(passthruPath);
    if (!lib) return 0;

    real.open = (OpenFn)GetProcAddress(lib, "PassThruOpen");
    real.close = (CloseFn)GetProcAddress(lib, "PassThruClose");
    real.connect = (ConnectFn)GetProcAddress(lib, "PassThruConnect");
    real.disconnect = (DisconnectFn)GetProcAddress(lib, "PassThruDisconnect");
    real.read = (MsgsFn)GetProcAddress(lib, "PassThruReadMsgs");
    real.write = (MsgsFn)GetProcAddress(lib, "PassThruWriteMsgs");
    real.startPeriodic = (StartPeriodicFn)GetProcAddress(lib, "PassThruStartPeriodicMsg");
    real.stopPeriodic = (StopFn)GetProcAddress(lib, "PassThruStopPeriodicMsg");
    real.startFilter = (StartFilterFn)GetProcAddress(lib, "PassThruStartMsgFilter");
    real.stopFilter = (StopFn)GetProcAddress(lib, "PassThruStopMsgFilter");
    real.voltage = (VoltageFn)GetProcAddress(lib, "PassThruSetProgrammingVoltage");
    real.version = (VersionFn)GetProcAddress(lib, "PassThruReadVersion");
    real.lastError = (LastErrorFn)GetProcAddress(lib, "PassThruGetLastError");
    real.ioctl = (IoctlFn)GetProcAddress(lib, "PassThruIoctl");

    if (!real.open || !real.close || !real.connect || !real.disconnect || !real.read || !real.write ||
        !real.startPeriodic || !real.stopPeriodic || !real.startFilter || !real.stopFilter ||
        !real.voltage || !real.version || !real.lastError || !real.ioctl) {
        FreeLibrary(lib);
        memset(&real, 0, sizeof real);
        return 0;
    }

    real.lib = lib;
    return 1;
}

// --- Wake-up frame -------------------------------------------------------------------------------

static HANDLE wakeThread;
static volatile LONG wakeRun;
static unsigned long wakeAnchor; // the ODIS channel whose connection started the wake-up
static unsigned long wakeCh;     // the channel the frame is sent on
static int wakeOwnCh;            // wakeCh was opened here (ODIS's channel is ISO15765, not raw CAN)

// crc8h2f is the AUTOSAR CRC-8 (polynomial 0x2F) the VW E2E protection uses, over data and then
// the message's data ID byte.
static unsigned char crc8h2f(const unsigned char *data, unsigned long n, unsigned char dataId) {
    unsigned char crc = 0xFF;
    for (unsigned long i = 0; i <= n; i++) {
        crc ^= i < n ? data[i] : dataId;
        for (int b = 0; b < 8; b++) crc = crc & 0x80 ? (unsigned char)(crc << 1 ^ 0x2F) : (unsigned char)(crc << 1);
    }
    return crc ^ 0xFF;
}

static DWORD WINAPI wakeLoop(LPVOID arg) {
    (void)arg;
    static PASSTHRU_MSG m;
    unsigned counter = 0, failures = 0;
    int was = -1;
    char flag[MAX_PATH];
    laertesFile(flag, sizeof flag, "wake.on");

    while (wakeRun) {
        int on = flag[0] && GetFileAttributesA(flag) != INVALID_FILE_ATTRIBUTES;
        if (on != was) {
            logStr(on ? "WAKE ignition on: sending the wake-up frame" : "WAKE ignition off: not sending");
            was = on;
        }
        if (!on) {
            Sleep(wakeMs);
            continue;
        }

        unsigned char d[8];
        memcpy(d, wakeData, wakeLen);
        // E2E: byte 1's low nibble counts 0..15, byte 0 is the CRC over bytes 1.. and the data ID.
        if (wakeE2E >= 0 && wakeLen >= 2) {
            d[1] = (unsigned char)((d[1] & 0xF0) | (counter & 0x0F));
            d[0] = crc8h2f(d + 1, wakeLen - 1, (unsigned char)wakeE2E);
        }
        counter++;

        if (real.lib) {
            memset(&m, 0, offsetof(PASSTHRU_MSG, Data));
            m.ProtocolID = CAN;
            m.TxFlags = wakeId > 0x7FF ? CAN_29BIT_ID : 0;
            m.DataSize = 4 + wakeLen;
            m.Data[0] = (unsigned char)(wakeId >> 24);
            m.Data[1] = (unsigned char)(wakeId >> 16);
            m.Data[2] = (unsigned char)(wakeId >> 8);
            m.Data[3] = (unsigned char)wakeId;
            memcpy(m.Data + 4, d, wakeLen);
            unsigned long n = 1;
            long err = real.write(wakeCh, &m, &n, 0);
            if (err && failures++ < 3) {
                char b[48];
                unsigned long o = 0;
                const char *p = "WAKE write failed err=";
                while (*p) b[o++] = *p++;
                putHexU32(b, &o, (unsigned long)err, 1);
                logWrite(b, o);
            }
        }
        canLog('W', wakeCh, wakeId, d, wakeLen);
        Sleep(wakeMs);
    }
    return 0;
}

// startWake starts the wake-up frame after ODIS connected channel ch, once per session.
static void startWake(unsigned long deviceId, unsigned long ch, unsigned long protocolId,
                      unsigned long baudrate) {
    if (!wakeOn || wakeThread) return;

    wakeCh = ch;
    wakeOwnCh = 0;
    if (real.lib && protocolId != CAN) {
        // An ISO15765 channel cannot send a raw frame: open a raw CAN channel next to it.
        if (real.connect(deviceId, CAN, CAN_ID_BOTH, baudrate, &wakeCh)) {
            logStr("WAKE could not open a raw CAN channel next to ODIS's channel");
            return;
        }
        wakeOwnCh = 1;
    }

    wakeAnchor = ch;
    wakeRun = 1;
    wakeThread = CreateThread(0, 0, wakeLoop, 0, 0, 0);
    logStr(real.lib ? "WAKE started on the real interface" : "WAKE started (simulated device: trace only)");
}

static void stopWake(void) {
    if (!wakeThread) return;

    wakeRun = 0;
    WaitForSingleObject(wakeThread, 2000);
    CloseHandle(wakeThread);
    wakeThread = 0;
    if (wakeOwnCh && real.lib) real.disconnect(wakeCh);
    wakeOwnCh = 0;
    logStr("WAKE stopped");
}

// --- J2534 entry points --------------------------------------------------------------------------

BOOL WINAPI DllMain(HINSTANCE inst, DWORD reason, LPVOID reserved) {
    (void)reserved;

    if (reason == DLL_PROCESS_ATTACH) {
        selfModule = inst;
        DisableThreadLibraryCalls(inst);
    }

    return TRUE;
}

long WINAPI PassThruOpen(void *name, unsigned long *deviceId) {
    (void)name;

    loadConfig();

    char self[MAX_PATH] = "", host[MAX_PATH] = "", b[2 * MAX_PATH + 32];
    GetModuleFileNameA(selfModule, self, MAX_PATH);
    GetModuleFileNameA(0, host, MAX_PATH);

    unsigned long o = 0;
    const char *p = "OPEN self=";
    while (*p) b[o++] = *p++;
    for (char *s = self; *s; s++) b[o++] = *s;

    p = " host=";
    while (*p) b[o++] = *p++;
    for (char *s = host; *s; s++) b[o++] = *s;

    if (passthruPath[0]) {
        p = " passthru=";
        while (*p) b[o++] = *p++;
        for (char *s = passthruPath; *s && o < sizeof b - 1; s++) b[o++] = *s;
    }

    logWrite(b, o);

    if (passthruPath[0]) {
        if (!loadReal()) {
            logStr("OPEN the real J2534 driver could not be loaded");
            return ERR_DEVICE_NOT_CONNECTED;
        }
        return real.open(name, deviceId);
    }

    return newId(deviceId);
}

long WINAPI PassThruClose(unsigned long deviceId) {
    stopWake();
    if (real.lib) return real.close(deviceId);
    return STATUS_NOERROR;
}

long WINAPI PassThruConnect(unsigned long deviceId, unsigned long protocolId, unsigned long flags,
                            unsigned long baudrate, unsigned long *channelId) {
    if (real.lib) {
        long err = real.connect(deviceId, protocolId, flags, baudrate, channelId);
        if (!err) startWake(deviceId, *channelId, protocolId, baudrate);
        return err;
    }

    unsigned long err = newId(channelId);
    if (err) return err;

    AcquireSRWLockExclusive(&lock);

    Channel *c = findChannel(0);
    if (c) {
        c->id = *channelId;
        c->proto = protocolId;
        c->nflt = 0;
        memset(&c->tp, 0, sizeof c->tp);
        c->loopback = 0;
    }

    char b[128];
    unsigned long o = 0;
    const char *p = "CONN ch=";
    while (*p) b[o++] = *p++;
    putHexU32(b, &o, *channelId, 1);
    p = " proto=";
    while (*p) b[o++] = *p++;
    putHexU32(b, &o, protocolId, 1);
    p = " baud=";
    while (*p) b[o++] = *p++;
    putHexU32(b, &o, baudrate, 1);
    logWrite(b, o);

    ReleaseSRWLockExclusive(&lock);

    startWake(deviceId, *channelId, protocolId, baudrate);
    return STATUS_NOERROR;
}

long WINAPI PassThruDisconnect(unsigned long channelId) {
    if (wakeThread && channelId == wakeAnchor) stopWake();
    if (real.lib) return real.disconnect(channelId);

    AcquireSRWLockExclusive(&lock);

    Channel *c = findChannel(channelId);
    if (c) c->id = 0;

    ReleaseSRWLockExclusive(&lock);
    return STATUS_NOERROR;
}

long WINAPI PassThruReadMsgs(unsigned long channelId, void *msgs, unsigned long *numMsgs,
                             unsigned long timeout) {
    if (real.lib) return real.read(channelId, msgs, numMsgs, timeout);
    if (!numMsgs) return ERR_NULL_PARAMETER;
    if (!msgs && *numMsgs) return ERR_NULL_PARAMETER;

    unsigned long cap = *numMsgs;
    unsigned long out = 0;
    PASSTHRU_MSG *m = (PASSTHRU_MSG *)msgs;

    if (timeout > 5000) timeout = 5000;
    DWORD start = GetTickCount();

    AcquireSRWLockExclusive(&lock);

    for (;;) {
        long long untilDue = -1;

        for (unsigned long i = 0; i < qCount && out < cap && msgs;) {
            Response *r = &queue[(qHead + i) % MAXQ];

            if (r->ch != channelId) {
                i++;
                continue;
            }

            long long wait = (long long)(r->due - usNow());
            if (wait > 0) {
                untilDue = wait;
                break;
            }

            PASSTHRU_MSG *dst = &m[out++];
            dst->ProtocolID = r->proto;
            dst->RxStatus = r->rxStatus;
            dst->TxFlags = 0;
            dst->Timestamp = r->ts;
            dst->DataSize = 4 + r->len;
            dst->ExtraDataIndex = 4 + r->len;
            dst->Data[0] = (unsigned char)(r->canId >> 24);
            dst->Data[1] = (unsigned char)(r->canId >> 16);
            dst->Data[2] = (unsigned char)(r->canId >> 8);
            dst->Data[3] = (unsigned char)r->canId;
            memcpy(dst->Data + 4, r->data, r->len);

            if (!r->rxStatus) {
                char b[16];
                unsigned long o = 0;
                b[o++] = 'D'; b[o++] = 'L'; b[o++] = 'V'; b[o++] = ' ';
                putHexU32(b, &o, channelId, 1);
                b[o++] = ' ';
                putHexU32(b, &o, r->canId, 3);
                logWrite(b, o);
            }

            for (unsigned long j = i; j + 1 < qCount; j++)
                queue[(qHead + j) % MAXQ] = queue[(qHead + j + 1) % MAXQ];
            qCount--;
        }

        DWORD waited = GetTickCount() - start;
        if (out || !msgs || waited >= timeout) break;

        DWORD sleep = timeout - waited;
        if (untilDue >= 0) {
            DWORD dueSleep = (DWORD)(untilDue / 1000 + 1);
            if (dueSleep < sleep) sleep = dueSleep;
        }

        SleepConditionVariableSRW(&queued, &lock, sleep, 0);
    }

    ReleaseSRWLockExclusive(&lock);

    *numMsgs = out;

    if (out == cap)
        return STATUS_NOERROR;

    if (out > 0 && timeout && (GetTickCount() - start) >= timeout)
        return ERR_TIMEOUT;

    return out ? STATUS_NOERROR : ERR_BUFFER_EMPTY;
}

long WINAPI PassThruWriteMsgs(unsigned long channelId, void *msgs, unsigned long *numMsgs,
                              unsigned long timeout) {
    if (real.lib) return real.write(channelId, msgs, numMsgs, timeout);

    if (!numMsgs) return ERR_NULL_PARAMETER;
    if (!msgs && *numMsgs) return ERR_NULL_PARAMETER;

    PASSTHRU_MSG *m = (PASSTHRU_MSG *)msgs;
    unsigned long processed = 0;

    AcquireSRWLockExclusive(&lock);

    Channel *c = findChannel(channelId);
    if (c && m) {
        for (unsigned long k = 0; k < *numMsgs; k++) {
            processed = k + 1;

            if (m[k].DataSize < 4) continue;

            unsigned long reqId = canId4(m[k].Data);
            canLog('R', channelId, reqId, m[k].Data + 4, m[k].DataSize - 4);

            if (c->loopback)
                enqueueAt(c, reqId, m[k].Data + 4, m[k].DataSize - 4, TX_MSG_TYPE, 0);

            if (c->proto != ISO15765) {
                handleCanFrame(c, reqId, m[k].Data + 4, m[k].DataSize - 4);
                continue;
            }

            unsigned long rspId = 0;
            for (unsigned long f = 0; f < c->nflt; f++)
                if (c->flt[f].tx == reqId) {
                    rspId = c->flt[f].rx;
                    break;
                }

            if (!rspId && c->nflt > 0) rspId = c->flt[0].rx;
            if (!rspId) continue;

            unsigned char out[MAXRESP];
            unsigned long len = buildResponse(m[k].Data + 4, m[k].DataSize - 4, out);

            if (len) {
                canLog('T', channelId, rspId, out, len);
                enqueue(c, rspId, out, len);
            }
        }
    }

    ReleaseSRWLockExclusive(&lock);

    *numMsgs = processed;
    return STATUS_NOERROR;
}

long WINAPI PassThruStartPeriodicMsg(unsigned long channelId, void *msg, unsigned long *msgId,
                                     unsigned long interval) {
    if (real.lib) return real.startPeriodic(channelId, msg, msgId, interval);

    PASSTHRU_MSG *m = (PASSTHRU_MSG *)msg;
    if (m && m->DataSize >= 4) {
        AcquireSRWLockExclusive(&lock);
        canLog('P', channelId, canId4(m->Data), m->Data + 4, m->DataSize - 4);
        ReleaseSRWLockExclusive(&lock);
    }

    return newId(msgId);
}

long WINAPI PassThruStopPeriodicMsg(unsigned long channelId, unsigned long msgId) {
    if (real.lib) return real.stopPeriodic(channelId, msgId);
    (void)channelId;
    (void)msgId;
    return STATUS_NOERROR;
}

long WINAPI PassThruStartMsgFilter(unsigned long channelId, unsigned long filterType, void *mask,
                                   void *pattern, void *flowControl, unsigned long *filterId) {
    if (real.lib) return real.startFilter(channelId, filterType, mask, pattern, flowControl, filterId);

    unsigned long err = newId(filterId);
    if (err) return err;

    PASSTHRU_MSG *msk = (PASSTHRU_MSG *)mask;
    PASSTHRU_MSG *pat = (PASSTHRU_MSG *)pattern;
    PASSTHRU_MSG *fc = (PASSTHRU_MSG *)flowControl;

    unsigned long maskId = 0, patId = 0, fcId = 0;
    if (msk && msk->DataSize >= 4) maskId = canId4(msk->Data);
    if (pat && pat->DataSize >= 4) patId = canId4(pat->Data);
    if (fc && fc->DataSize >= 4) fcId = canId4(fc->Data);

    canLogFilter(channelId, filterType, maskId, patId, fcId);

    if (pat && fc && pat->DataSize >= 4 && fc->DataSize >= 4) {
        AcquireSRWLockExclusive(&lock);

        Channel *c = findChannel(channelId);
        if (c && c->nflt < MAXF) {
            c->flt[c->nflt].tx = fcId;
            c->flt[c->nflt].rx = patId;
            c->nflt++;
        }

        ReleaseSRWLockExclusive(&lock);
    }

    return STATUS_NOERROR;
}

long WINAPI PassThruStopMsgFilter(unsigned long channelId, unsigned long filterId) {
    if (real.lib) return real.stopFilter(channelId, filterId);
    (void)channelId;
    (void)filterId;
    return STATUS_NOERROR;
}

long WINAPI PassThruSetProgrammingVoltage(unsigned long deviceId, unsigned long pin,
                                          unsigned long voltage) {
    if (real.lib) return real.voltage(deviceId, pin, voltage);
    (void)deviceId;
    (void)pin;
    (void)voltage;
    return STATUS_NOERROR;
}

long WINAPI PassThruReadVersion(unsigned long deviceId, char *firmware, char *dll, char *api) {
    if (real.lib) return real.version(deviceId, firmware, dll, api);

    if (!firmware || !dll || !api) return ERR_NULL_PARAMETER;

    lstrcpynA(firmware, "Laertes offline 1.0", 80);
    lstrcpynA(dll, "Laertes offline 1.0", 80);
    lstrcpynA(api, "04.04", 80);

    return STATUS_NOERROR;
}

long WINAPI PassThruGetLastError(char *description) {
    if (real.lib) return real.lastError(description);
    if (!description) return ERR_NULL_PARAMETER;
    lstrcpynA(description, "Laertes offline VCI: no interface, gateway simulated", 256);
    return STATUS_NOERROR;
}

long WINAPI PassThruIoctl(unsigned long id, unsigned long ioctlId, void *input, void *output) {
    if (real.lib) return real.ioctl(id, ioctlId, input, output);
    {
        char b[24];
        unsigned long o = 0;
        const char *p = "IOCTL ch=";
        while (*p) b[o++] = *p++;
        putHexU32(b, &o, id, 1);
        b[o++] = ' ';
        putHexU32(b, &o, ioctlId, 1);
        logWrite(b, o);
    }

    switch (ioctlId) {
    case READ_VBATT:
        if (!output) return ERR_NULL_PARAMETER;
        *(unsigned long *)output = 12000;
        return STATUS_NOERROR;

    case READ_PROG_VOLTAGE:
        if (!output) return ERR_NULL_PARAMETER;
        *(unsigned long *)output = 0;
        return STATUS_NOERROR;

    case GET_CONFIG: {
        SCONFIG_LIST *list = (SCONFIG_LIST *)input;
        if (!list || (list->NumOfParams && !list->ConfigPtr)) return ERR_NULL_PARAMETER;

        AcquireSRWLockExclusive(&lock);

        Channel *c = findChannel(id);
        for (unsigned long i = 0; i < list->NumOfParams; i++)
            list->ConfigPtr[i].Value =
                c && list->ConfigPtr[i].Parameter == LOOPBACK ? (unsigned long)c->loopback : 0;

        ReleaseSRWLockExclusive(&lock);
        return STATUS_NOERROR;
    }

    case SET_CONFIG: {
        SCONFIG_LIST *list = (SCONFIG_LIST *)input;
        if (!list || (list->NumOfParams && !list->ConfigPtr)) return ERR_NULL_PARAMETER;

        char b[192];
        unsigned long o = 0;
        const char *p = "SETCFG ch=";
        while (*p) b[o++] = *p++;
        putHexU32(b, &o, id, 1);

        AcquireSRWLockExclusive(&lock);

        Channel *c = findChannel(id);
        for (unsigned long i = 0; i < list->NumOfParams; i++) {
            if (list->ConfigPtr[i].Parameter == LOOPBACK && c)
                c->loopback = list->ConfigPtr[i].Value != 0;

            if (o + 32 < sizeof b) {
                b[o++] = ' ';
                putHexU32(b, &o, list->ConfigPtr[i].Parameter, 1);
                b[o++] = '=';
                putHexU32(b, &o, list->ConfigPtr[i].Value, 1);
            }
        }

        logWrite(b, o);
        ReleaseSRWLockExclusive(&lock);
        return STATUS_NOERROR;
    }

    default:
        return STATUS_NOERROR;
    }
}