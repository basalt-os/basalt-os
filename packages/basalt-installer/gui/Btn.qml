import QtQuick

// Button (from basalt-shell Btn.qml, without icons). Variants: ghost,
// primary, danger, outline.
Rectangle {
    id: b
    property string text: ""
    property string variant: "ghost"
    property bool enabled: true
    property string e2e: ""
    signal clicked()

    activeFocusOnTab: true
    implicitHeight: label.implicitHeight + Theme.s2 * 2
    implicitWidth: label.implicitWidth + Theme.s6
    radius: Theme.radiusSm
    opacity: enabled ? 1 : 0.45
    color: {
        if (variant === "primary") return ma.containsMouse ? Qt.lighter(Theme.accent, 1.08) : Theme.accent;
        if (variant === "danger") return Theme.danger;
        return ma.containsMouse ? Theme.hover : "transparent";
    }
    border.width: variant === "outline" || variant === "ghost" || activeFocus ? 1 : 0
    border.color: activeFocus ? Theme.accent : Theme.border

    Txt {
        id: label
        anchors.centerIn: parent
        text: b.text
        color: b.variant === "primary" || b.variant === "danger" ? Theme.accentText : Theme.text
        font.weight: b.variant === "primary" || b.variant === "danger" ? Font.DemiBold : Font.Medium
        wrapMode: Text.NoWrap
    }
    MouseArea {
        id: ma
        anchors.fill: parent
        hoverEnabled: true
        cursorShape: Qt.PointingHandCursor
        onClicked: if (b.enabled) b.clicked()
    }
    Keys.onReturnPressed: if (b.enabled) b.clicked()
    Keys.onEnterPressed: if (b.enabled) b.clicked()
    Keys.onSpacePressed: if (b.enabled) b.clicked()
}
