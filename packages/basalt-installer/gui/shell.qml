import QtQuick
import Quickshell

// Basalt OS installer, graphical frontend. Runs as the only client of a
// kiosk compositor (cage) on the live image and drives the same engine as
// the text installer through its local API.
ShellRoot {
    FloatingWindow {
        id: win
        title: "Basalt OS installer"
        color: Theme.bg
        implicitWidth: 1280
        implicitHeight: 800
        Installer { anchors.fill: parent }
    }
}
