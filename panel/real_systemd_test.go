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

	read := func() map[string]unitState {
		time.Sleep(2 * time.Second)
		unitStatesAt = time.Time{} // drop the 3s cache so the new state is read
		return loadUnitStates(append(running, "arstopped"))
	}

	// Wave 1: a schedule limited to arrun1 (plus the stopped arstopped, which
	// must stay stopped). arrun2 is running but NOT selected.
	(&app{}).restartAllRunning(autoRestartSettings{Only: []string{"arrun1", "arstopped"}})
	after1 := read()
	if after1["arrun1"].Active != "active" || after1["arrun1"].InvocationID == before["arrun1"].InvocationID {
		t.Errorf("selected arrun1 was not restarted: before %+v after %+v", before["arrun1"], after1["arrun1"])
	}
	if after1["arrun2"].InvocationID != before["arrun2"].InvocationID {
		t.Errorf("arrun2 was restarted although it was not selected")
	}

	// Wave 2: no selection = every location in the registry.
	(&app{}).restartAllRunning(autoRestartSettings{})
	after2 := read()
	for _, n := range []string{"arrun1", "arrun2"} {
		if after2[n].Active != "active" {
			t.Errorf("%s should be running after the restart: %+v", n, after2[n])
		}
	}
	if after2["arrun2"].InvocationID == before["arrun2"].InvocationID {
		t.Errorf("arrun2 was not restarted by the all-locations wave")
	}
	if after2["arrun1"].InvocationID == after1["arrun1"].InvocationID {
		t.Errorf("arrun1 was not restarted by the all-locations wave")
	}
	for _, after := range []map[string]unitState{after1, after2} {
		if after["arremoved"].InvocationID != before["arremoved"].InvocationID {
			t.Errorf("arremoved (not in the registry) was restarted")
		}
		if after["arstopped"].Active == "active" {
			t.Errorf("arstopped was started by the auto-restart: %+v", after["arstopped"])
		}
	}
}
