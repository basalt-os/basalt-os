import QtQuick

// Labeled single-line input (from basalt-shell Field.qml). secret masks it.
Column {
    id: f
    property alias text: input.text
    property string label: ""
    property string placeholder: ""
    property string help: ""
    property bool secret: false
    property bool mono: false
    signal accepted()
    spacing: Theme.s1
    width: parent ? parent.width : 400

    Txt { text: f.label; role: "small"; color: Theme.textMuted; visible: f.label !== "" }
    Rectangle {
        width: f.width
        implicitHeight: Theme.fontLarge * 2.6
        radius: Theme.radiusMd
        color: Theme.bg
        border.width: 1
        border.color: input.activeFocus ? Theme.accent : Theme.border
        TextInput {
            id: input
            anchors.left: parent.left
            anchors.right: parent.right
            anchors.margins: Theme.s3
            anchors.verticalCenter: parent.verticalCenter
            color: Theme.text
            selectionColor: Theme.accent
            selectedTextColor: Theme.accentText
            font.family: f.mono ? Theme.fontMono : Theme.fontFamily
            font.pointSize: f.mono ? Theme.fontSize : Theme.fontLarge
            echoMode: f.secret ? TextInput.Password : TextInput.Normal
            clip: true
            activeFocusOnTab: true
            onAccepted: f.accepted()
            Txt {
                anchors.fill: parent
                text: f.placeholder
                color: Theme.textMuted
                font.pointSize: parent.font.pointSize
                visible: input.text === "" && !input.activeFocus
                elide: Text.ElideRight
                wrapMode: Text.NoWrap
            }
        }
    }
    Txt { text: f.help; role: "small"; color: Theme.textMuted; visible: f.help !== ""; width: f.width }
}
