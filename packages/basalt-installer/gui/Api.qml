pragma Singleton

import QtQuick
import Quickshell
import Quickshell.Io

// Connection to the installer engine (basalt-installer serve): newline-
// delimited JSON over a Unix socket that only root and the live session
// user may open. The engine does every check again; this side only shows.
Singleton {
    id: api
    property bool connected: false
    property var events: []
    signal event(var e)

    property int _next: 1
    property var _callbacks: ({})

    readonly property string socketPath: Quickshell.env("BASALT_INSTALLER_SOCKET") || "/run/basalt-installer-api/api.sock"

    function call(op, args, cb) {
        if (!sock.connected) {
            if (cb) cb(false, "not connected to the installer engine");
            return;
        }
        const id = api._next++;
        if (cb) api._callbacks[id] = cb;
        sock.write(JSON.stringify({ id: id, op: op, args: args || {} }) + "\n");
        sock.flush();
    }

    function _handle(line) {
        let m;
        try { m = JSON.parse(line); } catch (e) { console.warn("installer: bad message", e); return; }
        if (m.event !== undefined) { api.event(m.event); return; }
        const cb = api._callbacks[m.id];
        if (cb) {
            delete api._callbacks[m.id];
            cb(m.ok, m.ok ? m.result : m.error);
        }
    }

    Socket {
        id: sock
        path: api.socketPath
        connected: true
        onConnectedChanged: {
            api.connected = connected;
            if (connected) api.call("subscribe", {});
        }
        parser: SplitParser { onRead: data => api._handle(data) }
    }

    // The engine service may start after the GUI.
    Timer {
        interval: 1000
        running: !sock.connected
        repeat: true
        onTriggered: { sock.connected = false; sock.connected = true; }
    }
}
