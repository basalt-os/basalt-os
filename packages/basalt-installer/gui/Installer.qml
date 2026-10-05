import QtQuick

// The installer flow: welcome, disk, options, accounts, review (the exact
// step list and a typed confirmation), install (progress, recovery key with
// acknowledgement), done. Every decision is made by the engine; this file
// only edits the plan and shows what the engine reports.
Item {
    id: root

    property var facts: null
    property var plan: null
    property var subvols: []
    property int page: 0
    readonly property var pages: ["Welcome", "Disk", "Options", "Accounts", "Review", "Install"]
    property string error: ""
    property var issues: []
    property var preview: null
    property string confirmText: ""

    // Installation state, from engine events.
    property bool running: false
    property bool finished: false
    property bool succeeded: false
    property real fraction: 0
    property string stepTitle: ""
    property string stepCommand: ""
    property var tail: []
    property string logPath: ""
    property string failure: ""
    property string recoveryKey: ""
    property bool recoveryAcked: false
    property string ackError: ""
    property bool askCancel: false
    // An existing /home (ADR 0002 addendum): candidates, inspection result.
    property var homeCands: []
    property var homeOwners: []
    property string homePass: ""
    property string homeNote: ""

    function targetDisk() {
        if (!root.facts || !root.plan) return null;
        return root.facts.disks.find(d => d.path === root.plan.target.disk) || null;
    }
    function largestFree(d) {
        return (d && d.free ? d.free : []).reduce((a, r) => Math.max(a, r.size_bytes), 0);
    }
    function existingESP(d) {
        const e = (d && d.partitions ? d.partitions : []).find(x => x.type === "c12a7328-f81f-11d2-ba4b-00a0c93ec93b" && x.fstype === "vfat");
        return e ? e.path : "";
    }
    // The home subvolume exists only while /home is not a partition of its own.
    function fixSubvols(p) {
        const elsewhere = (p.home && p.home.existing) || (p.encryption.scope === "home" && p.encryption.enabled !== false);
        let s = (p.layout.subvolumes || []).filter(x => x !== "home");
        if (!elsewhere) s.unshift("home");
        p.layout.subvolumes = s;
    }
    function loadHomeCands() {
        Api.call("home_candidates", {}, (ok, res) => {
            const d = root.plan ? root.plan.target : null;
            root.homeCands = ok ? (res || []).filter(c => !(d && c.disk === d.disk && d.wipe)) : [];
        });
    }
    function inspectHome() {
        root.homeNote = qsTr("Opening the partition read-only.");
        root.homeOwners = [];
        Api.call("inspect_home", { device: root.plan.home.existing.device, passphrase: root.homePass }, (ok, res) => {
            if (!ok) { root.homeNote = res; return; }
            root.homeOwners = res.owners || [];
            root.homeNote = root.homeOwners.length ? qsTr("Choose whose home it is: the new user gets the same name, uid and gid.") : qsTr("No home directories found on it.");
            edit(p => { p.home.existing.passphrase = root.homePass; });
        });
    }

    // Copies of the recovery key on removable media.
    property var keyMedia: []
    property bool keyMediaShown: false
    property string keySaveNote: ""

    function findKeyMedia() {
        root.keySaveNote = qsTr("Looking for USB sticks.");
        Api.call("key_media", {}, (ok, res) => {
            root.keyMediaShown = true;
            if (!ok) { root.keySaveNote = res; return; }
            root.keyMedia = res || [];
            root.keySaveNote = root.keyMedia.length ? qsTr("Choose where to write the key:") : qsTr("No USB stick with a FAT, exFAT or ext4 file system was found. Plug one in and look again.");
        });
    }
    function saveKey(dev) {
        root.keySaveNote = qsTr("Writing the key.");
        Api.call("save_recovery_key", { device: dev }, (ok, res) => {
            root.keySaveNote = ok ? qsTr("A copy is on %1. Keep that stick somewhere safe, then type the first group to confirm.").arg(res.saved) : qsTr("The key was not saved: %1").arg(res);
            if (ok) root.keyMediaShown = false;
        });
    }

    // Account fields that are not part of the plan until submitted.
    property string pw1: ""
    property string pw2: ""
    property string pp1: ""
    property string pp2: ""

    function edit(f) {
        if (!root.plan) return;
        const p = JSON.parse(JSON.stringify(root.plan));
        f(p);
        root.plan = p;
    }
    function human(b) {
        const u = ["B", "KiB", "MiB", "GiB", "TiB"];
        let i = 0;
        while (b >= 1024 && i < u.length - 1) { b /= 1024; i++; }
        return (i === 0 ? b : b.toFixed(1)) + " " + u[i];
    }
    function diskBlocker(d) {
        if (d.complex) return d.complex + ": use the kickstart installer";
        if (d.in_use) return "in use: " + d.in_use;
        if (d.read_only) return "read-only";
        if (d.size_bytes < 16 * 1024 * 1024 * 1024) return "smaller than 16 GiB";
        return "";
    }
    function keys(s) {
        s = (s || "").trim();
        if (s === "") return [];
        return s.split(/\n+/).map(x => x.trim()).filter(x => x !== "");
    }
    function load() {
        Api.call("suggest", {}, (ok, res) => {
            if (!ok) { root.error = res; return; }
            root.facts = res.facts;
            root.subvols = res.subvolumes;
            root.plan = res.plan;
        });
    }
    function makePreview() {
        root.error = "";
        root.preview = null;
        root.confirmText = "";
        if (root.pw1 !== root.pw2) { root.error = "The passwords do not match."; root.page = 3; return; }
        if (root.pp1 !== root.pp2) { root.error = "The disk passphrases do not match."; root.page = 2; return; }
        edit(p => {
            if (p.accounts.user) p.accounts.user.password = root.pw1;
            p.encryption.passphrase = p.encryption.enabled ? root.pp1 : "";
        });
        Api.call("preview", { plan: root.plan }, (ok, res) => {
            root.issues = (res && res.issues) ? res.issues : [];
            if (!ok) { root.error = res; return; }
            root.preview = res;
        });
    }
    function install() {
        root.error = "";
        root.tail = [];
        Api.call("install", { token: root.preview.token, confirm: root.confirmText }, (ok, res) => {
            if (!ok) { root.error = res; return; }
            root.running = true;
            root.finished = false;
            root.page = 5;
        });
    }
    function onEngineEvent(e) {
        switch (e.type) {
        case "started": root.logPath = e.log_path; break;
        case "step_start":
            root.stepTitle = "Step " + e.index + " of " + e.total + ": " + e.title;
            root.stepCommand = e.command;
            break;
        case "output": {
            let t = root.tail.slice();
            t.push(e.text);
            if (t.length > 14) t = t.slice(t.length - 14);
            root.tail = t;
            break;
        }
        case "progress": root.fraction = e.fraction; break;
        case "secret": if (e.kind === "recovery_key" && e.secret) root.recoveryKey = e.secret; break;
        case "recovery_key_acknowledged": root.recoveryAcked = true; root.recoveryKey = ""; root.keyMediaShown = false; root.keySaveNote = ""; break;
        case "done":
            root.running = false;
            root.finished = true;
            root.succeeded = e.ok === true;
            root.failure = e.error || "";
            if (e.log_path) root.logPath = e.log_path;
            if (root.succeeded) root.fraction = 1;
            else root.recoveryKey = ""; // the rolled-back volume's key opens nothing
            break;
        }
    }

    Connections {
        target: Api
        function onConnectedChanged() { if (Api.connected && !root.plan) root.load(); }
        function onEvent(e) { root.onEngineEvent(e); }
    }
    Component.onCompleted: if (Api.connected) load()

    // --- sidebar --------------------------------------------------------------------------
    Rectangle {
        id: side
        width: 280
        anchors { left: parent.left; top: parent.top; bottom: parent.bottom }
        color: Theme.surface
        Column {
            anchors { fill: parent; margins: Theme.s6 }
            spacing: Theme.s4
            Image {
                source: "file:///usr/share/pixmaps/basalt-logo-on-dark.svg"
                sourceSize.width: 180
                width: 180
                fillMode: Image.PreserveAspectFit
                visible: status === Image.Ready
            }
            Txt { text: "Basalt OS installer"; role: "large"; font.weight: Font.DemiBold; width: parent.width }
            Item { width: 1; height: Theme.s4 }
            Repeater {
                model: root.pages
                Row {
                    spacing: Theme.s3
                    Rectangle {
                        width: 26; height: 26; radius: 13
                        color: index === root.page ? Theme.accent : (index < root.page ? Theme.surfaceAlt : "transparent")
                        border.width: 1; border.color: index <= root.page ? Theme.accent : Theme.border
                        Txt { anchors.centerIn: parent; text: index + 1; role: "small"; color: index === root.page ? Theme.accentText : Theme.text }
                    }
                    Txt { text: modelData; anchors.verticalCenter: parent.verticalCenter; color: index === root.page ? Theme.text : Theme.textMuted }
                }
            }
        }
        Column {
            anchors { left: parent.left; right: parent.right; bottom: parent.bottom; margins: Theme.s6 }
            spacing: 2
            visible: root.facts !== null
            Txt { role: "small"; color: Theme.textMuted; width: parent.width; text: root.facts ? ("Firmware: " + (root.facts.uefi ? "UEFI" : "BIOS")) : "" }
            Txt { role: "small"; color: Theme.textMuted; width: parent.width; text: root.facts ? ("Secure Boot: " + root.facts.secure_boot) : "" }
            Txt { role: "small"; color: Theme.textMuted; width: parent.width; text: root.facts ? ("TPM 2.0: " + (root.facts.tpm2 ? "yes" : "no")) : "" }
            Txt { role: "small"; color: Theme.textMuted; width: parent.width; text: root.facts ? ("Machine: " + (root.facts.virt || "bare metal")) : "" }
            Txt { role: "small"; color: Api.connected ? Theme.success : Theme.danger; width: parent.width; text: Api.connected ? "Engine connected" : "Waiting for the engine" }
        }
    }

    // --- page area ------------------------------------------------------------------------------
    Flickable {
        id: flick
        anchors { left: side.right; right: parent.right; top: parent.top; bottom: bar.top; margins: Theme.s6 }
        contentHeight: pageCol.implicitHeight
        clip: true
        Column {
            id: pageCol
            width: Math.min(flick.width, 900)
            spacing: Theme.s4

            // 0 welcome
            Column {
                visible: root.page === 0
                width: parent.width
                spacing: Theme.s4
                Txt { text: "Install Basalt OS"; role: "display"; width: parent.width }
                Txt {
                    width: parent.width
                    text: qsTr("A Fedora remix with server and desktop editions: SELinux enforcing, btrfs with a snapshot before every update, LUKS2 encryption unlocked by the TPM, and a local assistant that only proposes changes.") + "\n\n" + qsTr("You choose a disk and a few settings, then review the exact list of commands before anything is written. Nothing changes until you type the disk name.")
                }
                Txt { width: parent.width; role: "small"; color: Theme.textMuted; text: "The Fedora packages come from the network. A text installer with the same steps runs on the serial console." }
            }

            // 1 disk
            Column {
                visible: root.page === 1 && root.plan !== null
                width: parent.width
                spacing: Theme.s3
                Txt { text: "Where should Basalt OS be installed?"; role: "title"; width: parent.width }
                Txt { width: parent.width; color: Theme.textMuted; text: "The whole disk is used and erased. One local disk only: RAID, multipath and iSCSI installs use the kickstart installer." }
                Repeater {
                    model: root.facts ? root.facts.disks : []
                    Choice {
                        readonly property string blocker: root.diskBlocker(modelData)
                        width: pageCol.width
                        title: modelData.path + "    " + root.human(modelData.size_bytes) + (modelData.model ? "    " + modelData.model : "")
                        detail: blocker !== "" ? "Not available: " + blocker : modelData.contents
                        enabled: blocker === ""
                        checked: root.plan && root.plan.target.disk === modelData.path
                        onPicked: root.edit(p => { p.target.disk = modelData.path; p.target.wipe = true; })
                    }
                }
            }

            // 2 options
            Column {
                visible: root.page === 2 && root.plan !== null
                width: parent.width
                spacing: Theme.s6
                Section {
                    title: qsTr("Disk use")
                    visible: { const d = root.targetDisk(); return d !== null && d.table === "gpt" && (d.partitions || []).length > 0; }
                    detail: { const d = root.targetDisk(); return d ? qsTr("%1 holds partitions: %2").arg(d.path).arg((d.partitions || []).map(x => x.path + " " + root.human(x.size_bytes) + " " + (x.fstype || "")).join(", ")) : ""; }
                    Choice { width: pageCol.width; title: qsTr("Erase the whole disk"); detail: qsTr("Everything on it is deleted")
                        checked: root.plan && root.plan.target.wipe
                        onPicked: root.edit(p => { p.target.wipe = true; delete p.target.esp; }) }
                    Choice { width: pageCol.width; title: qsTr("Keep the partitions, use the free space (dual boot)")
                        detail: qsTr("Free: %1. The other systems and their EFI partition are not touched.").arg(root.human(root.largestFree(root.targetDisk())))
                        checked: root.plan && !root.plan.target.wipe && !root.plan.target.esp
                        onPicked: root.edit(p => { p.target.wipe = false; delete p.target.esp; }) }
                    Choice { width: pageCol.width; visible: root.existingESP(root.targetDisk()) !== ""
                        title: qsTr("Keep the partitions, share the EFI partition %1").arg(root.existingESP(root.targetDisk()))
                        detail: qsTr("Fedora's boot files go into EFI/fedora on it")
                        checked: root.plan && !root.plan.target.wipe && !!root.plan.target.esp
                        onPicked: root.edit(p => { p.target.wipe = false; p.target.esp = root.existingESP(root.targetDisk()); }) }
                }
                Section {
                    title: qsTr("Home directories (/home)")
                    Choice { width: pageCol.width; title: qsTr("A new /home on this disk"); checked: root.plan && !(root.plan.home && root.plan.home.existing)
                        onPicked: root.edit(p => { p.home = {}; root.fixSubvols(p); }) }
                    Choice { width: pageCol.width; title: qsTr("An existing /home partition: keep its files")
                        detail: qsTr("It is never formatted: it is opened, mounted on /home and set up to open at boot")
                        checked: root.plan && !!(root.plan.home && root.plan.home.existing)
                        onPicked: { root.loadHomeCands(); root.edit(p => { p.home = { existing: { device: "", tpm2: false } }; if (p.encryption.scope === "home") p.encryption.scope = "system"; root.fixSubvols(p); }); } }
                    Repeater {
                        model: root.plan && root.plan.home && root.plan.home.existing ? root.homeCands : []
                        Choice {
                            width: pageCol.width
                            title: modelData.path + "    " + root.human(modelData.size_bytes) + "    " + modelData.fstype + (modelData.label ? "    " + modelData.label : "")
                            checked: root.plan.home.existing.device === modelData.path
                            onPicked: { root.homeOwners = []; root.homeNote = ""; root.edit(p => { p.home.existing.device = modelData.path; }); }
                        }
                    }
                    Row {
                        visible: root.plan && root.plan.home && root.plan.home.existing && root.plan.home.existing.device !== ""
                        width: parent.width; spacing: Theme.s3
                        Field { width: (parent.width - Theme.s3) / 2; secret: true; label: qsTr("Its passphrase (checked now, never stored)"); onTextChanged: root.homePass = text; onAccepted: root.inspectHome() }
                        Btn { text: qsTr("Open read-only"); variant: "outline"; anchors.bottom: parent.bottom; onClicked: root.inspectHome() }
                    }
                    Txt { width: parent.width; color: Theme.textMuted; text: root.homeNote; visible: root.homeNote !== "" }
                    Repeater {
                        model: root.homeOwners
                        Choice {
                            width: pageCol.width
                            title: "/home/" + modelData.name; detail: "uid " + modelData.uid + ", gid " + modelData.gid
                            checked: root.plan && root.plan.accounts.user && root.plan.accounts.user.uid === modelData.uid && root.plan.accounts.user.name === modelData.name
                            onPicked: root.edit(p => {
                                p.accounts.user = p.accounts.user || { admin: true };
                                p.accounts.user.name = modelData.name; p.accounts.user.uid = modelData.uid; p.accounts.user.gid = modelData.gid;
                                p.accounts.user.home_dir = "/home/" + modelData.name + "-basalt";
                            })
                        }
                    }
                    Field { width: parent.width; label: qsTr("Home directory of the new user")
                        visible: root.plan && root.plan.home && root.plan.home.existing && root.plan.accounts.user
                        help: qsTr("A directory of its own keeps Basalt OS settings apart from the other system's. Nothing in the other directories is changed.")
                        text: root.plan && root.plan.accounts.user ? (root.plan.accounts.user.home_dir || "") : ""
                        onTextChanged: if (root.plan && root.plan.accounts.user && text !== (root.plan.accounts.user.home_dir || "")) root.edit(p => { p.accounts.user.home_dir = text; }) }
                    Check { width: pageCol.width; visible: root.plan && root.plan.home && root.plan.home.existing && root.facts && root.facts.tpm2
                        title: qsTr("Also open it with this machine's TPM"); detail: qsTr("The passphrase keeps working for the other system")
                        checked: root.plan && root.plan.home && root.plan.home.existing && root.plan.home.existing.tpm2 === true
                        onToggled: on => root.edit(p => { p.home.existing.tpm2 = on; }) }
                }
                Section {
                    title: "Partitioning"
                    detail: "EFI system partition, /boot, and one btrfs file system with subvolumes, so data survives a system rollback."
                    Choice { title: "Automatic (recommended)"; detail: "The Basalt layout on the whole disk"; checked: root.plan && root.plan.layout.mode === "automatic"
                        onPicked: root.edit(p => { p.layout = { mode: "automatic", esp_mib: 600, boot_mib: 1024, subvolumes: root.subvols.map(s => s.name) }; root.fixSubvols(p); }) }
                    Choice { title: "Manual"; detail: "Choose the sizes and the optional subvolumes"; checked: root.plan && root.plan.layout.mode === "manual"
                        onPicked: root.edit(p => { p.layout.mode = "manual"; }) }
                    Row {
                        visible: root.plan && root.plan.layout.mode === "manual"
                        width: parent.width
                        spacing: Theme.s3
                        Field { width: (parent.width - 2 * Theme.s3) / 3; label: "EFI partition (MiB)"; placeholder: "600"
                            onTextChanged: root.edit(p => { p.layout.esp_mib = parseInt(text) || 600; }) }
                        Field { width: (parent.width - 2 * Theme.s3) / 3; label: "/boot (MiB)"; placeholder: "1024"
                            onTextChanged: root.edit(p => { p.layout.boot_mib = parseInt(text) || 1024; }) }
                        Field { width: (parent.width - 2 * Theme.s3) / 3; label: "System (GiB)"; placeholder: "rest of the disk"
                            onTextChanged: root.edit(p => { p.layout.root_gib = parseInt(text) || 0; }) }
                    }
                    Repeater {
                        model: root.plan && root.plan.layout.mode === "manual" ? root.subvols : []
                        Check {
                            width: pageCol.width
                            title: modelData.name + "   " + modelData.mountpoint
                            detail: modelData.purpose
                            locked: modelData.required
                            checked: root.plan.layout.subvolumes.indexOf(modelData.name) >= 0
                            onToggled: on => root.edit(p => {
                                const s = p.layout.subvolumes.filter(x => x !== modelData.name);
                                if (on) s.push(modelData.name);
                                p.layout.subvolumes = s;
                            })
                        }
                    }
                }
                Section {
                    title: "Disk encryption (LUKS2)"
                    detail: root.facts && !root.facts.tpm2 ? "No TPM 2.0 was found on this machine." : "A recovery key is always generated and shown once."
                    Repeater {
                        model: [
                            { v: "system", t: qsTr("Encrypt the whole system (recommended)"), d: qsTr("Everything on the disk: the system, its settings, the logs and the users' files") },
                            { v: "home", t: qsTr("Encrypt only /home"), d: qsTr("The users' files only: the system, its settings in /etc, the logs and /var (containers, virtual machines, databases) stay unencrypted") },
                            { v: "none", t: qsTr("Encrypt nothing"), d: qsTr("Only for servers in a controlled place") }
                        ]
                        Choice {
                            width: pageCol.width
                            visible: !(modelData.v === "home" && root.plan && root.plan.home && root.plan.home.existing)
                            title: modelData.t; detail: modelData.d
                            checked: root.plan && (modelData.v === "none" ? root.plan.encryption.enabled === false : (root.plan.encryption.enabled !== false && (root.plan.encryption.scope || "system") === modelData.v))
                            onPicked: root.edit(p => {
                                p.encryption.scope = modelData.v;
                                p.encryption.enabled = modelData.v !== "none";
                                if (modelData.v === "none") p.encryption.tang = {};
                                root.fixSubvols(p);
                            })
                        }
                    }
                    Repeater {
                        model: [
                            { u: "tpm2", t: "Unlock with the TPM (recommended)", d: "Sealed to the Secure Boot state (PCR 7): the disk opens by itself while the boot chain is unchanged" },
                            { u: "recovery-only", t: "Unlock with the recovery key", d: "Typed at every boot" },
                            { u: "tang", t: "Unlock from a Tang server", d: "Network-bound encryption (Clevis)" },
                            { u: "tpm2+tang", t: "TPM and Tang, both required", d: "Opens only on this machine and on this network" }
                        ]
                        Choice {
                            width: pageCol.width
                            visible: root.plan && root.plan.encryption.enabled !== false
                            title: modelData.t; detail: modelData.d
                            checked: root.plan && root.plan.encryption.enabled !== false && root.plan.encryption.unlock === modelData.u
                            onPicked: root.edit(p => {
                                p.encryption.unlock = modelData.u;
                                if (modelData.u.indexOf("tang") < 0) p.encryption.tang = {};
                            })
                        }
                    }
                    Row {
                        visible: root.plan && root.plan.encryption.enabled !== false && root.plan.encryption.unlock.indexOf("tang") >= 0
                        width: parent.width; spacing: Theme.s3
                        Field { width: (parent.width - Theme.s3) / 2; label: "Tang URL"; placeholder: "http://tang.example:7500"; text: root.plan && root.plan.encryption.tang ? (root.plan.encryption.tang.url || "") : ""
                            onTextChanged: if (root.plan) root.edit(p => { p.encryption.tang = p.encryption.tang || {}; p.encryption.tang.url = text; }) }
                        Field { width: (parent.width - Theme.s3) / 2; label: "Thumbprint (recommended)"; text: root.plan && root.plan.encryption.tang ? (root.plan.encryption.tang.thumbprint || "") : ""
                            onTextChanged: if (root.plan) root.edit(p => { p.encryption.tang = p.encryption.tang || {}; p.encryption.tang.thumbprint = text; }) }
                    }
                    Row {
                        visible: root.plan && root.plan.encryption.enabled !== false
                        width: parent.width; spacing: Theme.s3
                        Field { width: (parent.width - Theme.s3) / 2; secret: true; label: "Boot passphrase (optional, desktops)"; onTextChanged: root.pp1 = text }
                        Field { width: (parent.width - Theme.s3) / 2; secret: true; label: "Repeat the passphrase"; onTextChanged: root.pp2 = text }
                    }
                }
                Section {
                    title: "System"
                    Row {
                        width: parent.width; spacing: Theme.s3
                        Field { width: (parent.width - Theme.s3) / 2; label: "Host name"; text: root.plan ? root.plan.hostname : ""; onTextChanged: if (root.plan && text !== root.plan.hostname) root.edit(p => { p.hostname = text; }) }
                        Field { width: (parent.width - Theme.s3) / 2; label: "Time zone"; text: root.plan ? root.plan.timezone : ""; onTextChanged: if (root.plan && text !== root.plan.timezone) root.edit(p => { p.timezone = text; }) }
                    }
                    Repeater {
                        model: [
                            { v: "auto", t: "Packages: automatic", d: "Minimal on a virtual machine, standard on bare metal" },
                            { v: "minimal", t: "Packages: minimal", d: "No hardware firmware, CPU microcode or fwupd" },
                            { v: "standard", t: "Packages: standard", d: "With firmware, microcode and fwupd" }
                        ]
                        Choice { width: pageCol.width; title: modelData.t; detail: modelData.d; checked: root.plan && root.plan.profile === modelData.v; onPicked: root.edit(p => { p.profile = modelData.v; }) }
                    }
                }
                Section {
                    title: "Network"
                    Choice { width: pageCol.width; title: "Automatic (DHCP)"; detail: "On every wired interface"; checked: root.plan && root.plan.network.mode === "dhcp"; onPicked: root.edit(p => { p.network = { mode: "dhcp" }; }) }
                    Choice { width: pageCol.width; title: "Static address"; detail: root.facts ? "Interfaces: " + (root.facts.interfaces || []).join(", ") : ""; checked: root.plan && root.plan.network.mode === "static"
                        onPicked: root.edit(p => {
                            p.network.mode = "static";
                            // The detected interface (the first of several) is filled in.
                            if (!p.network.interface && root.facts && root.facts.interfaces && root.facts.interfaces.length) p.network.interface = root.facts.interfaces[0];
                        }) }
                    Row {
                        visible: root.plan && root.plan.network.mode === "static"
                        width: parent.width; spacing: Theme.s3
                        Field { width: (parent.width - 3 * Theme.s3) / 4; label: qsTr("Interface"); text: root.plan && root.plan.network.interface ? root.plan.network.interface : ""
                            onTextChanged: if (root.plan && text !== (root.plan.network.interface || "")) root.edit(p => { p.network.interface = text; }) }
                        Field { width: (parent.width - 3 * Theme.s3) / 4; label: "Address/prefix"; placeholder: "192.0.2.10/24"; onTextChanged: root.edit(p => { p.network.address = text; }) }
                        Field { width: (parent.width - 3 * Theme.s3) / 4; label: "Gateway"; onTextChanged: root.edit(p => { p.network.gateway = text; }) }
                        Field { width: (parent.width - 3 * Theme.s3) / 4; label: "DNS (spaces)"; onTextChanged: root.edit(p => { p.network.dns = text.split(/\s+/).filter(x => x !== ""); }) }
                    }
                }
                Section {
                    title: "Repositories"
                    detail: "Packages and metadata are signature checked. basalt and Fedora are always on."
                    Check { width: pageCol.width; title: "basalt"; detail: "Basalt OS packages: security and identity updates"; checked: true; locked: true }
                    Check { width: pageCol.width; title: "basalt-tools"; detail: "OpenBasalt tools: metadata only, nothing installed unless chosen"; checked: root.plan && root.plan.repos.tools !== false; onToggled: on => root.edit(p => { p.repos.tools = on; }) }
                    Check { width: pageCol.width; title: "tui-tools (third party)"; detail: "Terminal tools with a command preview, from upstream; key fingerprint pinned"; checked: root.plan && root.plan.repos.third_party.tui_tools !== false; onToggled: on => root.edit(p => { p.repos.third_party.tui_tools = on; }) }
                    Field { label: "Basalt repository for the install"; help: "\"media\" is the repository on this installer image"; text: root.plan ? root.plan.repos.basalt.url : ""; onTextChanged: if (root.plan && text !== root.plan.repos.basalt.url) root.edit(p => { p.repos.basalt.url = text; }) }
                }
            }

            // 3 accounts
            Column {
                visible: root.page === 3 && root.plan !== null
                width: parent.width
                spacing: Theme.s4
                Txt { text: "Accounts"; role: "title"; width: parent.width }
                Txt { width: parent.width; color: Theme.textMuted; text: "Give root an SSH key, create an administrator, or both. SSH accepts keys only unless you allow passwords below." }
                Field { mono: true; label: "Root SSH public key (empty: root stays locked)"
                    placeholder: root.plan && root.plan.accounts.root.ssh_keys && root.plan.accounts.root.ssh_keys.length ? root.plan.accounts.root.ssh_keys.length + " key(s) from the plan file" : "ssh-ed25519 AAAA... you@host"
                    onTextChanged: if (root.plan && text !== "") root.edit(p => { p.accounts.root.ssh_keys = root.keys(text); }) }
                Field { label: "Administrator user name (optional, group wheel)"; placeholder: root.plan && root.plan.accounts.user ? root.plan.accounts.user.name : ""
                    onTextChanged: if (root.plan) root.edit(p => { if (text === "") { delete p.accounts.user; return; } p.accounts.user = p.accounts.user || { admin: true }; p.accounts.user.name = text; }) }
                Row {
                    width: parent.width; spacing: Theme.s3
                    visible: root.plan && root.plan.accounts.user
                    Field { width: (parent.width - Theme.s3) / 2; secret: true; label: "Password"; onTextChanged: root.pw1 = text }
                    Field { width: (parent.width - Theme.s3) / 2; secret: true; label: "Repeat the password"; onTextChanged: root.pw2 = text }
                }
                Field { mono: true; visible: root.plan && root.plan.accounts.user; label: "Administrator SSH public key (optional)"
                    onTextChanged: if (root.plan && root.plan.accounts.user) root.edit(p => { p.accounts.user.ssh_keys = root.keys(text); }) }
                Check { width: parent.width; title: "Allow password logins over SSH"; detail: "Not recommended: Basalt OS accepts public keys only"; checked: root.plan && root.plan.ssh.password_auth === true
                    onToggled: on => root.edit(p => { p.ssh.password_auth = on; }) }
            }

            // 4 review
            Column {
                visible: root.page === 4
                width: parent.width
                spacing: Theme.s3
                Txt { text: root.preview ? "Review: " + root.preview.steps + " steps, exactly what will run" : "Review"; role: "title"; width: parent.width }
                Repeater {
                    model: root.issues
                    Txt { width: pageCol.width; role: "small"; color: modelData.severity === "error" ? Theme.danger : Theme.warning; text: modelData.severity + ": " + modelData.field + ": " + modelData.message }
                }
                Rectangle {
                    width: parent.width
                    height: 380
                    radius: Theme.radiusMd
                    color: Theme.surface
                    border.width: 1; border.color: Theme.border
                    visible: root.preview !== null
                    Flickable {
                        id: pv
                        anchors { fill: parent; margins: Theme.s3 }
                        contentHeight: pvText.implicitHeight
                        contentWidth: width
                        clip: true
                        Text {
                            id: pvText
                            width: pv.width
                            text: root.preview ? root.preview.text : ""
                            color: Theme.text
                            font.family: Theme.fontMono
                            font.pointSize: Theme.fontSmall
                            wrapMode: Text.WrapAnywhere
                            textFormat: Text.PlainText
                        }
                    }
                }
                Rectangle {
                    width: parent.width
                    implicitHeight: confirmCol.implicitHeight + Theme.s4 * 2
                    radius: Theme.radiusMd
                    color: Qt.rgba(Theme.danger.r, Theme.danger.g, Theme.danger.b, 0.10)
                    border.width: 1; border.color: Theme.danger
                    visible: root.preview !== null
                    Column {
                        id: confirmCol
                        anchors { left: parent.left; right: parent.right; top: parent.top; margins: Theme.s4 }
                        spacing: Theme.s3
                        Txt { width: parent.width; font.weight: Font.DemiBold; text: root.preview ? "Everything on " + root.preview.resolved.disk.path + " (" + root.preview.resolved.disk.contents + ") will be erased." : "" }
                        Field { width: parent.width; label: root.preview ? "Type " + root.preview.confirm_word + " to confirm" : ""; text: root.confirmText; onTextChanged: root.confirmText = text }
                        Btn { text: "Erase and install"; variant: "danger"; enabled: root.preview && root.confirmText === root.preview.confirm_word; onClicked: root.install() }
                    }
                }
            }

            // 5 install
            Column {
                visible: root.page === 5
                width: parent.width
                spacing: Theme.s4
                Txt { role: "title"; width: parent.width; text: root.finished ? (root.succeeded ? "Basalt OS is installed" : "The installation failed") : "Installing Basalt OS" }
                Rectangle {
                    width: parent.width; height: 12; radius: 6; color: Theme.surfaceAlt
                    Rectangle { width: parent.width * root.fraction; height: parent.height; radius: 6; color: root.finished && !root.succeeded ? Theme.danger : Theme.accent }
                }
                Txt { width: parent.width; text: Math.round(root.fraction * 100) + " %   " + root.stepTitle; visible: !root.finished }
                Txt { width: parent.width; role: "mono"; color: Theme.success; text: root.stepCommand ? "$ " + root.stepCommand : ""; visible: !root.finished; wrapMode: Text.WrapAnywhere }
                Txt { width: parent.width; color: Theme.danger; text: root.failure; visible: root.failure !== "" }
                Txt { width: parent.width; visible: root.finished && root.succeeded
                    text: "Install record on the new system: /var/log/basalt-installer/. The first boot takes the first snapshot; the disk unlocks by itself while Secure Boot is unchanged." }
                Rectangle {
                    width: parent.width; height: 260; radius: Theme.radiusMd; color: Theme.surface; border.width: 1; border.color: Theme.border
                    Column {
                        anchors { fill: parent; margins: Theme.s3 }
                        Repeater { model: root.tail
                            Text { width: parent.width; text: modelData; color: Theme.textMuted; font.family: Theme.fontMono; font.pointSize: Theme.fontSmall; elide: Text.ElideRight } }
                    }
                }
                Txt { width: parent.width; role: "small"; color: Theme.textMuted; text: "Log: " + root.logPath; visible: root.logPath !== "" }
            }

            Txt { width: parent.width; color: Theme.danger; text: root.error; visible: root.error !== "" }
        }
    }

    // --- bottom bar ----------------------------------------------------------------------------
    Rectangle {
        id: bar
        anchors { left: side.right; right: parent.right; bottom: parent.bottom }
        height: 72
        color: Theme.surface
        Row {
            anchors { right: parent.right; verticalCenter: parent.verticalCenter; rightMargin: Theme.s6 }
            spacing: Theme.s3
            Btn { text: "Back"; visible: root.page > 0 && root.page < 5; onClicked: { root.error = ""; root.page-- } }
            Btn {
                text: root.page === 3 ? "Review the plan" : "Continue"
                variant: "primary"
                visible: root.page < 4
                enabled: root.plan !== null && (root.page !== 1 || root.plan.target.disk !== "")
                onClicked: {
                    root.error = "";
                    if (root.page === 3) root.makePreview();
                    root.page++;
                }
            }
            Btn { text: "Cancel installation"; variant: "outline"; visible: root.page === 5 && root.running; onClicked: root.askCancel = true }
            Btn { text: "Back to the review"; visible: root.page === 5 && root.finished && !root.succeeded; onClicked: { root.page = 4; root.makePreview(); } }
            Btn {
                text: root.plan && root.plan.finish === "poweroff" ? "Power off" : "Restart now"
                variant: "primary"
                visible: root.page === 5 && root.finished && root.succeeded
                enabled: root.recoveryAcked || (root.plan && root.plan.encryption.enabled === false)
                onClicked: Api.call("finish", {}, (ok, res) => { if (!ok) root.error = res; })
            }
        }
    }

    // --- overlays ---------------------------------------------------------------------------------
    Rectangle {
        anchors.fill: parent
        color: "#b30a0c0f"
        visible: root.recoveryKey !== "" || root.askCancel
        MouseArea { anchors.fill: parent }
        Rectangle {
            visible: root.recoveryKey !== ""
            anchors.centerIn: parent
            width: Math.min(parent.width - 80, 760)
            implicitHeight: rk.implicitHeight + Theme.s6 * 2
            radius: Theme.radiusLg
            color: Theme.surfaceAlt
            border.width: 1; border.color: Theme.accent
            Column {
                id: rk
                anchors { left: parent.left; right: parent.right; top: parent.top; margins: Theme.s6 }
                spacing: Theme.s4
                Txt { text: "Recovery key: write it down now"; role: "title"; color: Theme.warning; width: parent.width }
                Txt { width: parent.width; text: qsTr("This key opens the disk when the TPM refuses (Secure Boot changed, the disk moved to another machine). It is shown only this once and is not stored on any disk unless you save a copy. Keep it off this machine.") }
                Rectangle {
                    width: parent.width; implicitHeight: keyText.implicitHeight + Theme.s4 * 2; radius: Theme.radiusMd; color: Theme.bg
                    Text {
                        id: keyText
                        anchors { left: parent.left; right: parent.right; verticalCenter: parent.verticalCenter; margins: Theme.s4 }
                        text: root.recoveryKey.split("-").reduce((acc, g, i) => acc + (i === 0 ? "" : (i % 4 === 0 ? "-\n" : "-")) + g, "")
                        color: Theme.text; font.family: Theme.fontMono; font.pointSize: Theme.fontTitle; horizontalAlignment: Text.AlignHCenter
                    }
                }
                Field { id: proof; label: "Type the first group to confirm you stored it"; mono: true; onAccepted: ackBtn.clicked() }
                Txt { width: parent.width; color: Theme.danger; text: root.ackError; visible: root.ackError !== "" }
                Row {
                    spacing: Theme.s3
                    Btn { id: ackBtn; text: "I stored the recovery key"; variant: "primary"
                        onClicked: Api.call("ack_recovery_key", { proof: proof.text }, (ok, res) => { root.ackError = ok ? "" : res; }) }
                    Btn { text: qsTr("Save a copy to a USB stick"); variant: "outline"; onClicked: root.findKeyMedia() }
                }
                Txt { width: parent.width; color: Theme.textMuted; text: root.keySaveNote; visible: root.keySaveNote !== "" }
                Repeater {
                    model: root.keyMediaShown ? root.keyMedia : []
                    Btn {
                        text: modelData.path + "   " + root.human(modelData.size_bytes) + "   " + (modelData.label || modelData.fstype) + (modelData.model ? "   " + modelData.model : "")
                        onClicked: root.saveKey(modelData.path)
                    }
                }
            }
        }
        Rectangle {
            visible: root.askCancel
            anchors.centerIn: parent
            width: 560; implicitHeight: cc.implicitHeight + Theme.s6 * 2
            radius: Theme.radiusLg; color: Theme.surfaceAlt; border.width: 1; border.color: Theme.danger
            Column {
                id: cc
                anchors { left: parent.left; right: parent.right; top: parent.top; margins: Theme.s6 }
                spacing: Theme.s4
                Txt { text: "Cancel the installation?"; role: "title"; width: parent.width }
                Txt { width: parent.width; text: "The running step is stopped and what can be undone is undone (mounts, the open encrypted volume, the temporary key). The disk is already partitioned and will not boot." }
                Row { spacing: Theme.s3
                    Btn { text: "Keep installing"; onClicked: root.askCancel = false }
                    Btn { text: "Cancel and roll back"; variant: "danger"; onClicked: { root.askCancel = false; Api.call("cancel", {}); } }
                }
            }
        }
    }
}
