package main

import (
	"os"
	"os/exec"
	"testing"
	"time"
)

// Runs only with REGIONHOP_REAL_SYSTEMD=1 (CI, as root, on a machine with
// systemd): starts a transient psi-tunnel@citest unit that logs a
// Psiphon-style connect notice followed by 300 noise lines -- enough to push
// it out of the 200-line window -- and checks the real systemctl/journalctl
// paths still report it as active in the right region.
func TestRealSystemd(t *testing.T) {
	if os.Getenv("REGIONHOP_REAL_SYSTEMD") != "1" {
		t.Skip("REGIONHOP_REAL_SYSTEMD not set")
	}
	unit := "psi-tunnel@citest.service"
	script := `echo '{"data":{"count":1},"noticeType":"Tunnels","timestamp":"2026-01-01T00:00:00.000Z"}'
echo '{"data":{"serverRegion":"US"},"noticeType":"ConnectedServerRegion","timestamp":"2026-01-01T00:00:00.001Z"}'
i=0; while [ $i -lt 300 ]; do echo '{"data":{"message":"noise"},"noticeType":"Info","timestamp":"2026-01-01T00:00:01.000Z"}'; i=$((i+1)); done
sleep 120`
	if out, err := exec.Command("systemd-run", "--unit="+unit, "sh", "-c", script).CombinedOutput(); err != nil {
		t.Fatalf("systemd-run: %v\n%s", err, out)
	}
	defer exec.Command("systemctl", "stop", unit).Run()
	time.Sleep(3 * time.Second)

	states := loadUnitStates([]string{"citest", "nonexistent"})
	st := states["citest"]
	if st.Active != "active" || st.InvocationID == "" {
		t.Fatalf("citest state = %+v (all: %+v)", st, states)
	}
	if ne, ok := states["nonexistent"]; !ok || ne.Active != "inactive" {
		t.Fatalf("nonexistent state = %+v ok=%v (all: %+v)", ne, ok, states)
	}

	out, _ := tunnelLogs("citest", 200)
	if _, _, found := scanNotices(out); found {
		t.Fatal("expected the connect notice to be outside the 200-line window")
	}
	state, region := tunnelRunInfo("citest", st.InvocationID)
	if state != "active" || region != "US" {
		t.Fatalf("tunnelRunInfo = (%q, %q), want (active, US)", state, region)
	}
	info := tunnelStatusInfo("citest", st)
	if info.State != "active" || info.Region != "US" {
		t.Fatalf("tunnelStatusInfo = %+v", info)
	}
}

// Auto restart against real units (REGIONHOP_REAL_SYSTEMD=1, CI as root):
// two running locations must be restarted (new invocation ID), a location in
// the registry that is stopped must stay stopped, and a running unit that is
// NOT in the registry (removed mid-wave) must be left alone. Goes through
// the real `sudo -n systemctl restart` path the panel uses.
func TestRealSystemdAutoRestart(t *testing.T) {
	if os.Getenv("REGIONHOP_REAL_SYSTEMD") != "1" {
		t.Skip("REGIONHOP_REAL_SYSTEMD not set")
	}
	if err := os.MkdirAll("/opt/psi-panel/data", 0o755); err != nil {
		t.Fatal(err)
	}
	reg := `[{"name":"arrun1","region":"","socks_port":19101},{"name":"arrun2","region":"","socks_port":19102},{"name":"arstopped","region":"","socks_port":19103}]`
	if err := os.WriteFile(registryPath, []byte(reg), 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(registryPath)

	running := []string{"arrun1", "arrun2", "arremoved"}
	for _, n := range running {
		unit := unitName(n)
		if out, err := exec.Command("systemd-run", "--unit="+unit, "sleep", "300").CombinedOutput(); err != nil {
			t.Fatalf("systemd-run %s: %v\n%s", unit, err, out)
		}
		defer exec.Command("systemctl", "stop", unit).Run()
	}
	time.Sleep(2 * time.Second)

	before := loadUnitStates(append(running, "arstopped"))
	for _, n := range running {
		if before[n].Active != "active" || before[n].InvocationID == "" {
			t.Fatalf("%s should be running before the wave: %+v", n, before[n])
		}
	}
	if before["arstopped"].Active == "active" {
		t.Fatalf("arstopped must not be running: %+v", before["arstopped"])
	}

	(&app{}).restartAllRunning()
	time.Sleep(2 * time.Second)

	unitStatesAt = time.Time{} // drop the 3s cache so the new state is read
	after := loadUnitStates(append(running, "arstopped"))
	for _, n := range []string{"arrun1", "arrun2"} {
		if after[n].Active != "active" {
			t.Errorf("%s should be running after the restart: %+v", n, after[n])
		}
		if after[n].InvocationID == before[n].InvocationID {
			t.Errorf("%s was not restarted (same invocation %s)", n, before[n].InvocationID)
		}
	}
	if after["arremoved"].InvocationID != before["arremoved"].InvocationID {
		t.Errorf("arremoved (not in the registry) was restarted")
	}
	if after["arstopped"].Active == "active" {
		t.Errorf("arstopped was started by the auto-restart: %+v", after["arstopped"])
	}
}
