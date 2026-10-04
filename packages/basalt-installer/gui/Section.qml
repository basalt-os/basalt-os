import QtQuick

// A titled group on a page.
Column {
    property string title: ""
    property string detail: ""
    default property alias content: inner.data
    width: parent ? parent.width : 600
    spacing: Theme.s2
    Txt { text: parent.title; role: "large"; font.weight: Font.DemiBold; width: parent.width }
    Txt { text: parent.detail; role: "small"; color: Theme.textMuted; width: parent.width; visible: parent.detail !== "" }
    Column { id: inner; width: parent.width; spacing: Theme.s2 }
}
