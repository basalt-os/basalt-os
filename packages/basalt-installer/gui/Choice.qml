import QtQuick

// One option of a single-choice group: a card with a title and a detail.
Rectangle {
    id: c
    property string title: ""
    property string detail: ""
    property bool checked: false
    property bool enabled: true
    property string e2e: ""
    signal picked()
    width: parent ? parent.width : 400
    implicitHeight: col.implicitHeight + Theme.s3 * 2
    radius: Theme.radiusMd
    color: checked ? Theme.accentSoft : (ma.containsMouse ? Theme.hover : Theme.surfaceAlt)
    border.width: 1
    border.color: checked ? Theme.accent : (activeFocus ? Theme.accent : Theme.border)
    opacity: enabled ? 1 : 0.5
    activeFocusOnTab: true

    Rectangle {
        id: dot
        width: Theme.s4; height: width; radius: width / 2
        anchors.left: parent.left; anchors.leftMargin: Theme.s3
        anchors.top: parent.top; anchors.topMargin: Theme.s3 + 2
        color: "transparent"
        border.width: 2
        border.color: c.checked ? Theme.accent : Theme.textMuted
        Rectangle { anchors.centerIn: parent; width: parent.width / 2; height: width; radius: width / 2; color: Theme.accent; visible: c.checked }
    }
    Column {
        id: col
        anchors.left: dot.right; anchors.leftMargin: Theme.s3
        anchors.right: parent.right; anchors.rightMargin: Theme.s3
        anchors.verticalCenter: parent.verticalCenter
        spacing: 2
        Txt { text: c.title; width: parent.width; font.weight: Font.Medium }
        Txt { text: c.detail; width: parent.width; role: "small"; color: Theme.textMuted; visible: c.detail !== "" }
    }
    MouseArea { id: ma; anchors.fill: parent; hoverEnabled: true; onClicked: if (c.enabled) c.picked() }
    Keys.onSpacePressed: if (c.enabled) c.picked()
    Keys.onReturnPressed: if (c.enabled) c.picked()
}
