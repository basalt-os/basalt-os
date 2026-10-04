pragma Singleton

import QtQuick
import Quickshell

// Design tokens of the installer GUI: a static copy of the Basalt dark theme
// of the desktop shell (basalt-shell themes/basalt.json, shell/Theme.qml).
// The installer has no shell daemon to read live tokens from. Once the shell
// publishes its tokens as a shared QML module, this file imports it instead.
Singleton {
    readonly property color bg: "#111418"
    readonly property color surface: "#181c21"
    readonly property color surfaceAlt: "#22272e"
    readonly property color border: "#343b44"
    readonly property color text: "#ece7e1"
    readonly property color textMuted: "#a0a6ae"
    readonly property color accent: "#b5502f"
    readonly property color accentText: "#ffffff"
    readonly property color success: "#5fae7b"
    readonly property color warning: "#d9a23a"
    readonly property color danger: "#e0605a"
    readonly property color hover: Qt.rgba(text.r, text.g, text.b, 0.08)
    readonly property color accentSoft: Qt.rgba(accent.r, accent.g, accent.b, 0.16)

    readonly property string fontFamily: "Inter"
    readonly property string fontMono: "JetBrains Mono"
    readonly property real fontSize: 11
    readonly property real scale: 1.2
    readonly property real fontSmall: Math.round(fontSize / scale * 10) / 10
    readonly property real fontLarge: Math.round(fontSize * scale * 10) / 10
    readonly property real fontTitle: Math.round(fontSize * scale * scale * 10) / 10
    readonly property real fontDisplay: Math.round(fontSize * scale * scale * scale * 10) / 10

    readonly property real radiusSm: 6
    readonly property real radiusMd: 10
    readonly property real radiusLg: 16
    readonly property real unit: 4
    readonly property real s1: unit
    readonly property real s2: unit * 2
    readonly property real s3: unit * 3
    readonly property real s4: unit * 4
    readonly property real s6: unit * 6
    readonly property int fast: 120
}
