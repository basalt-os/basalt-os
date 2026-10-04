import QtQuick

// Text in the theme's typography (from basalt-shell Txt.qml).
Text {
    property string role: "body" // body, small, large, title, display, mono
    color: Theme.text
    font.family: role === "mono" ? Theme.fontMono : Theme.fontFamily
    font.pointSize: {
        switch (role) {
        case "small": return Theme.fontSmall;
        case "large": return Theme.fontLarge;
        case "title": return Theme.fontTitle;
        case "display": return Theme.fontDisplay;
        case "mono": return Theme.fontSmall;
        default: return Theme.fontSize;
        }
    }
    font.weight: role === "title" || role === "display" ? Font.DemiBold : Font.Normal
    wrapMode: Text.Wrap
}
