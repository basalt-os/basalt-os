# Basalt OS snapshots: snapper for the root file system, a pre and a post
# snapshot around every dnf transaction, GRUB entries that boot snapshots,
# and rollback.

Name:           basalt-snapshots
Version:        0.3.0
Release:        1%{?dist}
Summary:        Snapshots of the root file system around every dnf transaction
License:        Apache-2.0
URL:            https://github.com/basalt-os/basalt-os
BuildArch:      noarch

Source0:        LICENSE
Source1:        basalt-root
Source2:        basalt-snapper.actions
Source3:        basalt-snapshot-dnf
Source4:        basalt-snapshot-boot
Source5:        basalt-snapshots-setup
Source6:        basalt-rollback
Source7:        42_basalt_snapshots
Source8:        basalt-snapshot-boot.service
Source9:        snapper-cleanup-basalt.conf
Source10:       snapshots.conf
Source11:       basalt-initial-snapshot.service
Source12:       basalt-snapshots-auto.service
Source13:       80-basalt-snapshots.preset

BuildRequires:  systemd-rpm-macros
Requires:       snapper
Requires:       btrfs-progs
# dnf5 hook: the actions plugin runs basalt-snapshot-dnf around transactions.
Requires:       libdnf5-plugin-actions
Requires:       grub2-tools
Requires:       grubby
Requires:       util-linux
Requires:       gawk
Requires:       sed
%{?systemd_requires}

%description
Basalt OS keeps the root file system on a btrfs subvolume and takes a
snapper snapshot before and after every dnf transaction (installs, updates,
removals), through the libdnf5 actions plugin. Old snapshots are removed by
a retention policy. The newest snapshots are listed in the GRUB menu and
boot read-only; basalt-rollback (snapper rollback) makes a snapshot the new
root. /home and other data subvolumes are never rolled back.

%prep
%setup -q -c -T
cp -p %{sources} .

%build

%install
install -Dpm 0644 basalt-root %{buildroot}%{_datadir}/snapper/config-templates/basalt-root
install -Dpm 0644 basalt-snapper.actions %{buildroot}%{_sysconfdir}/dnf/libdnf5-plugins/actions.d/basalt-snapper.actions
install -Dpm 0755 basalt-snapshot-dnf %{buildroot}%{_libexecdir}/basalt/basalt-snapshot-dnf
install -Dpm 0755 basalt-snapshot-boot %{buildroot}%{_bindir}/basalt-snapshot-boot
install -Dpm 0755 basalt-snapshots-setup %{buildroot}%{_bindir}/basalt-snapshots-setup
install -Dpm 0755 basalt-rollback %{buildroot}%{_bindir}/basalt-rollback
install -Dpm 0755 42_basalt_snapshots %{buildroot}%{_sysconfdir}/grub.d/42_basalt_snapshots
install -Dpm 0644 basalt-snapshot-boot.service %{buildroot}%{_unitdir}/basalt-snapshot-boot.service
install -Dpm 0644 basalt-initial-snapshot.service %{buildroot}%{_unitdir}/basalt-initial-snapshot.service
install -Dpm 0644 basalt-snapshots-auto.service %{buildroot}%{_unitdir}/basalt-snapshots-auto.service
install -Dpm 0644 80-basalt-snapshots.preset %{buildroot}%{_presetdir}/80-basalt-snapshots.preset
install -Dpm 0644 snapper-cleanup-basalt.conf %{buildroot}%{_unitdir}/snapper-cleanup.service.d/basalt.conf
install -Dpm 0644 snapshots.conf %{buildroot}%{_sysconfdir}/basalt/snapshots.conf
install -d licenses && install -pm 0644 LICENSE licenses/

%pre
# An upgrade from a version without basalt-snapshots-auto.service: %%post
# applies its preset once (%%systemd_post does only on a first install),
# so the systems that need it most, upgraded ones without snapshots, get it.
if [ $1 -gt 1 ] && [ ! -e %{_unitdir}/basalt-snapshots-auto.service ]; then
    mkdir -p /run/basalt-snapshots && touch /run/basalt-snapshots/preset-auto || :
fi

%post
%systemd_post basalt-snapshot-boot.service basalt-initial-snapshot.service basalt-snapshots-auto.service
if [ -e /run/basalt-snapshots/preset-auto ]; then
    systemctl --no-reload preset basalt-snapshots-auto.service >/dev/null 2>&1 || :
    rm -f /run/basalt-snapshots/preset-auto
fi
# Refresh the menu on upgrades of this package (the generator may change).
if [ $1 -gt 1 ] && [ -d /.snapshots ] && [ -d /boot/grub2 ]; then
    %{_bindir}/basalt-snapshot-boot update >/dev/null 2>&1 || :
fi

%posttrans
# Installed on a running system that has no snapshots (a Fedora install
# that got the Basalt packages afterwards): say so now, not only in the
# journal at the next boot. The installer and the kickstart set snapshots
# up themselves after the packages, from a chroot, where this stays quiet.
if [ -d /run/systemd/system ] && [ ! -e /etc/snapper/configs/root ] &&
    ! systemd-detect-virt -q --chroot 2>/dev/null; then
    out=$(%{_bindir}/basalt-snapshots-setup --auto --check 2>&1)
    rc=$?
    [ -n "$out" ] && printf '%s\n' "$out" >&2
    if [ "$rc" = 0 ]; then
        echo "basalt-snapshots: this system has no snapshots yet. They are set up at the next boot (basalt-snapshots-auto.service), or now with: sudo basalt-snapshots-setup" >&2
    fi
fi
:

%preun
%systemd_preun basalt-snapshot-boot.service basalt-initial-snapshot.service basalt-snapshots-auto.service

%postun
%systemd_postun basalt-snapshot-boot.service basalt-initial-snapshot.service basalt-snapshots-auto.service
if [ $1 -eq 0 ]; then
    rm -f /boot/grub2/basalt-snapshots.cfg || :
fi

%files
%license licenses/LICENSE
%dir %{_datadir}/snapper
%dir %{_datadir}/snapper/config-templates
%{_datadir}/snapper/config-templates/basalt-root
%config(noreplace) %{_sysconfdir}/dnf/libdnf5-plugins/actions.d/basalt-snapper.actions
%dir %{_libexecdir}/basalt
%{_libexecdir}/basalt/basalt-snapshot-dnf
%{_bindir}/basalt-snapshot-boot
%{_bindir}/basalt-snapshots-setup
%{_bindir}/basalt-rollback
%{_sysconfdir}/grub.d/42_basalt_snapshots
%{_unitdir}/basalt-snapshot-boot.service
%{_unitdir}/basalt-initial-snapshot.service
%{_unitdir}/basalt-snapshots-auto.service
%{_presetdir}/80-basalt-snapshots.preset
%dir %{_unitdir}/snapper-cleanup.service.d
%{_unitdir}/snapper-cleanup.service.d/basalt.conf
%dir %{_sysconfdir}/basalt
%config(noreplace) %{_sysconfdir}/basalt/snapshots.conf

%changelog
* Wed Oct 07 2026 Basalt OS project <noreply@basalt-os.org> - 0.3.0-1
- A system that got the Basalt packages after it was installed (plain
  Fedora) had no snapper configuration and nothing said so: dnf
  transactions took no snapshots and rollback did not work.
  basalt-snapshots-auto.service (enabled by its own preset) now sets the
  snapshots up at the next boot when the root is a btrfs subvolume booted
  by GRUB, and otherwise writes why not to the journal; installing the
  package says the same right away. basalt-snapshots-setup --auto and
  --check; SETUP=off in snapshots.conf turns the automatic setup off.
  Upgrades from 0.2 get the unit's preset too.

* Sun Oct 04 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-4
- Snapshot menu entries and their submenu carry the basalt-snapshot class
  first, so the Basalt GRUB theme shows them with the snapshot icon.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-3
- basalt-rollback --clean-kernels: also remove the dangling symvers link.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-2
- basalt-rollback: when started from a snapshot booted from the menu, hand
  over to a newer basalt-rollback of the installed system, if there is one.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.2.0-1
- basalt-rollback --kernels, --clean-kernels, --list-orphans: kernels left
  on /boot by a rollback that no package and no snapshot needs any more are
  reported after every dnf transaction and removed on request.
- Boot menu: hotkey "s" opens the snapshot submenu.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-8
- Detect a boot from the snapshot menu by basalt.snapshot= on the kernel
  command line: after a rollback the normal root is a snapshot subvolume too.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-7
- The first snapshot is taken on the first boot (basalt-initial-snapshot.service),
  not inside the installer, where files are not labeled yet.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-6
- basalt-rollback: make the newest kernel whose modules exist in the
  rolled-back root the default boot entry.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-5
- Boot menu: leave out the snapshot that is the current default subvolume.
- Template: no background comparison (memory peak on large updates).

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-4
- Create snapshots through snapperd when D-Bus is available, so its cached
  snapshot list stays current.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-3
- GRUB hook: drop the unexpanded ${extra_cmdline} placeholder from boot entries.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-2
- Setup: set SUSE_BTRFS_SNAPSHOT_BOOTING so grub2-mkconfig stops adding
  rootflags=subvol= to boot entries.

* Sat Oct 03 2026 Basalt OS project <noreply@basalt-os.org> - 0.1.0-1
- First version: snapper template, dnf5 actions hook, snapshot boot menu,
  setup and rollback helpers.
