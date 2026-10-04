import QtQuick

// A checkbox row.
Rectangle {
    id: k
    property string title: ""
    property string detail: ""
    property bool checked: false
    property bool locked: false
    signal toggled(bool on)
    width: parent ? parent.width : 400
    implicitHeight: col.implicitHeight + Theme.s2 * 2
    radius: Theme.radiusSm
    color: ma.containsMouse && !locked ? Theme.hover : "transparent"
    activeFocusOnTab: !locked
    border.width: activeFocus ? 1 : 0
    border.color: Theme.accent

    Rectangle {
        id: box
        width: Theme.s4; height: width; radius: 3
        anchors.left: parent.left; anchors.leftMargin: Theme.s2
        anchors.top: parent.top; anchors.topMargin: Theme.s2 + 2
        color: k.checked ? (k.locked ? Theme.textMuted : Theme.accent) : "transparent"
        border.width: 2
        border.color: k.checked ? "transparent" : Theme.textMuted
        Txt { anchors.centerIn: parent; text: "✓"; color: Theme.accentText; role: "small"; visible: k.checked }
    }
    Column {
        id: col
        anchors.left: box.right; anchors.leftMargin: Theme.s3
        anchors.right: parent.right
        anchors.verticalCenter: parent.verticalCenter
        Txt { text: k.title + (k.locked ? "  (required)" : ""); width: parent.width; color: k.locked ? Theme.textMuted : Theme.text }
        Txt { text: k.detail; width: parent.width; role: "small"; color: Theme.textMuted; visible: k.detail !== "" }
    }
    MouseArea { id: ma; anchors.fill: parent; hoverEnabled: true; onClicked: if (!k.locked) k.toggled(!k.checked) }
    Keys.onSpacePressed: if (!k.locked) k.toggled(!k.checked)
}
