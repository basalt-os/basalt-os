package action

import (
	"strings"
	"testing"
)

func TestCommandsAreExact(t *testing.T) {
	cases := []struct {
		a    Action
		want []string
	}{
		{Action{SELinuxFcontext, map[string]string{"path": "/srv/nginx-logs", "type": "httpd_log_t"}},
			[]string{`semanage fcontext -a -t httpd_log_t '/srv/nginx-logs(/.*)?'`, "restorecon -Rv /srv/nginx-logs"}},
		{Action{SELinuxFcontext, map[string]string{"path": "/srv/my.site", "type": "httpd_sys_content_t"}},
			[]string{`semanage fcontext -a -t httpd_sys_content_t '/srv/my\.site(/.*)?'`, "restorecon -Rv /srv/my.site"}},
		{Action{SELinuxPort, map[string]string{"proto": "tcp", "port": "8099", "type": "http_port_t", "mode": "add"}},
			[]string{"semanage port -a -t http_port_t -p tcp 8099"}},
		{Action{SELinuxBoolean, map[string]string{"name": "httpd_can_network_connect", "value": "on"}},
			[]string{"setsebool -P httpd_can_network_connect on"}},
		{Action{FileRestore, map[string]string{"path": "/etc/nginx/nginx.conf", "snapshot": "3"}},
			[]string{"cp --preserve=mode,ownership,timestamps /.snapshots/3/snapshot/etc/nginx/nginx.conf /etc/nginx/nginx.conf", "restorecon -v /etc/nginx/nginx.conf"}},
		{Action{SnapshotRollback, map[string]string{"snapshot": "12"}}, []string{"basalt-rollback --yes 12"}},
		{Action{JournalVacuum, map[string]string{"size": "200M"}}, []string{"journalctl --vacuum-size=200M"}},
	}
	for _, c := range cases {
		cmds, err := c.a.Commands()
		if err != nil {
			t.Fatalf("%s: %v", c.a.Kind, err)
		}
		var got []string
		for _, x := range cmds {
			got = append(got, x.String())
		}
		if strings.Join(got, "\n") != strings.Join(c.want, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", c.a.Kind, got, c.want)
		}
		if len(c.a.Verify(timeZero)) == 0 {
			t.Errorf("%s: no verification checks", c.a.Kind)
		}
	}
}

func TestValidationRejects(t *testing.T) {
	bad := []Action{
		{SELinuxFcontext, map[string]string{"path": "/srv/x; rm -rf /", "type": "httpd_log_t"}},
		{SELinuxFcontext, map[string]string{"path": "/srv/../etc", "type": "httpd_log_t"}},
		{SELinuxFcontext, map[string]string{"path": "/", "type": "httpd_log_t"}},
		{SELinuxFcontext, map[string]string{"path": "/srv/x", "type": "httpd_log_t -R"}},
		{SELinuxRestorecon, map[string]string{"path": "relative/path"}},
		{SELinuxPort, map[string]string{"proto": "tcp", "port": "70000", "type": "http_port_t", "mode": "add"}},
		{SELinuxPort, map[string]string{"proto": "icmp", "port": "80", "type": "http_port_t", "mode": "add"}},
		{SELinuxBoolean, map[string]string{"name": "x y", "value": "on"}},
		{SELinuxBoolean, map[string]string{"name": "httpd_x", "value": "1; reboot"}},
		{UnitRestart, map[string]string{"unit": "nginx.service --now"}},
		{UnitRestart, map[string]string{"unit": "nginx"}},
		{FileRestore, map[string]string{"path": "/root/.ssh/authorized_keys", "snapshot": "3"}},
		{FileRestore, map[string]string{"path": "/etc/passwd", "snapshot": "-1"}},
		{SnapshotDelete, map[string]string{"snapshot": "0"}},
		{JournalVacuum, map[string]string{"size": "all"}},
		{"shell.run", map[string]string{"cmd": "id"}},
	}
	for _, a := range bad {
		if _, err := a.Commands(); err == nil {
			t.Errorf("accepted %s %v", a.Kind, a.Params)
		}
	}
}
