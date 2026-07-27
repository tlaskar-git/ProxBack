package pve_test

import (
	"encoding/json"
	"strings"
	"testing"

	"proxback/internal/pve"
)

func TestParseDisks(t *testing.T) {
	cfg := map[string]any{
		"name":     "web-01",
		"cores":    2,
		"scsi1":    "local-lvm:vm-100-disk-1,size=32G",
		"scsi0":    "local-lvm:vm-100-disk-0,size=16M",
		"virtio0":  "ceph:vm-100-disk-2,size=1T,discard=on",
		"ide2":     "local:iso/debian.iso,media=cdrom,size=512M",
		"efidisk0": "local-lvm:vm-100-disk-3,size=1M",
		"net0":     "virtio=AA:BB:CC:DD:EE:FF,bridge=vmbr0",
		"unused0":  "local-lvm:vm-100-disk-9",
	}
	disks := pve.ParseDisks(cfg)
	var names []string
	for _, d := range disks {
		names = append(names, d.Name)
	}
	// cdrom media, efidisk, unused volumes and non-disk keys are all excluded,
	// and the result is ordered by config key.
	want := []string{"scsi0", "scsi1", "virtio0"}
	if len(names) != len(want) {
		t.Fatalf("disks = %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("disks = %v, want %v", names, want)
		}
	}
	if disks[0].SizeBytes != 16<<20 {
		t.Errorf("scsi0 size = %d, want %d", disks[0].SizeBytes, 16<<20)
	}
	if disks[1].SizeBytes != 32<<30 {
		t.Errorf("scsi1 size = %d, want %d", disks[1].SizeBytes, 32<<30)
	}
	if disks[2].SizeBytes != 1<<40 {
		t.Errorf("virtio0 size = %d, want %d", disks[2].SizeBytes, 1<<40)
	}
	if disks[0].Volume != "local-lvm:vm-100-disk-0" {
		t.Errorf("scsi0 volume = %q", disks[0].Volume)
	}
}

func TestParseTags(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", []string{}},
		{"prod;web", []string{"prod", "web"}},
		// Trimmed, lower-cased, sorted, empties dropped.
		{" Web ;;PROD; ", []string{"prod", "web"}},
		{"zeta;alpha;mid", []string{"alpha", "mid", "zeta"}},
		// Duplicates collapse, and commas are tolerated alongside semicolons.
		{"dev;dev,Dev", []string{"dev"}},
		{";", []string{}},
	}
	for _, c := range cases {
		got := pve.ParseTags(c.in)
		if got == nil {
			t.Fatalf("ParseTags(%q) returned nil, want an empty slice", c.in)
		}
		if strings.Join(got, "|") != strings.Join(c.want, "|") {
			t.Errorf("ParseTags(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestVMDecodesTags(t *testing.T) {
	// Exactly the shape a PVE guest listing returns: tags is one semicolon
	// separated string, not an array.
	const raw = `[
		{"vmid":100,"name":"web-01","status":"running","maxdisk":33554432,"maxmem":2147483648,
		 "uptime":3600,"tags":"prod;web"},
		{"vmid":102,"name":"app-01","status":"running","maxdisk":16777216,"maxmem":2147483648,
		 "uptime":3600}
	]`
	var vms []pve.VM
	if err := json.Unmarshal([]byte(raw), &vms); err != nil {
		t.Fatalf("decode guest listing: %v", err)
	}
	if len(vms) != 2 {
		t.Fatalf("decoded %d guests, want 2", len(vms))
	}
	if vms[0].Name != "web-01" || vms[0].VMID != 100 || vms[0].MaxDisk != 32<<20 {
		t.Fatalf("guest fields lost while decoding tags: %+v", vms[0])
	}
	if strings.Join(vms[0].Tags, ",") != "prod,web" {
		t.Fatalf("web-01 tags = %v, want [prod web]", vms[0].Tags)
	}
	// A guest with no tags field must still yield an empty slice, never null.
	if vms[1].Tags == nil || len(vms[1].Tags) != 0 {
		t.Fatalf("untagged guest tags = %#v, want an empty slice", vms[1].Tags)
	}
}

func TestAuthHeaderShape(t *testing.T) {
	c, err := pve.New(pve.Config{
		BaseURL: "https://pve.example:8006/", TokenID: "root@pam!proxback", TokenSecret: "abc",
	})
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	if got, want := c.AuthHeader(), "PVEAPIToken=root@pam!proxback=abc"; got != want {
		t.Fatalf("AuthHeader = %q, want %q", got, want)
	}
	if _, err := pve.New(pve.Config{}); err == nil {
		t.Fatal("client without a base URL was accepted")
	}
}

func TestTaskStatusHelpers(t *testing.T) {
	running := pve.TaskStatus{Status: "running"}
	if running.Done() || running.OK() {
		t.Fatal("a running task reported done/ok")
	}
	ok := pve.TaskStatus{Status: "stopped", ExitStatus: "OK"}
	if !ok.Done() || !ok.OK() {
		t.Fatal("a successful task did not report done/ok")
	}
	failed := pve.TaskStatus{Status: "stopped", ExitStatus: "command failed"}
	if !failed.Done() || failed.OK() {
		t.Fatal("a failed task reported ok")
	}
}
