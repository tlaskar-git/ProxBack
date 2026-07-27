// Package pvesim is a Proxmox VE API simulator covering the subset of the API
// that ProxBack uses, plus sim-only endpoints for E2E assertions.
//
// It serves 2 nodes and 4 guests whose disk contents are deterministic
// pseudo-random data, supports snapshots (which capture the disk content at that
// moment), streams disks through the ProxBack export extension, accepts restore
// streams through the import extension and can deterministically mutate a
// fraction of one disk so incremental backups can be proven.
package pvesim

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// DiskSize is the size of every simulated disk.
const DiskSize = 16 << 20 // 16 MiB == 4 engine chunks

// ChunkSize mirrors the engine chunk size; mutations are chunk aligned so tests
// can assert exactly how many chunks changed.
const ChunkSize = 4 << 20

// MutateFraction is the fraction of a disk's chunks rewritten by /sim/mutate.
const MutateFraction = 4 // one quarter

type simVM struct {
	VMID      int
	Name      string
	Node      string
	Status    string
	Tags      string // PVE-style semicolon separated tag list
	MaxMem    int64
	Uptime    int64
	DiskNames []string
	Content   map[string][]byte
	Snapshots map[string]map[string][]byte
	MutGen    map[string]uint64
}

// Sim is a running simulator.
type Sim struct {
	log *slog.Logger

	mu       sync.Mutex
	nodes    []string
	vms      map[int]*simVM
	imported map[string][]byte
	tasks    map[string]string
	authSeen []string
	taskSeq  uint64
}

// New builds a simulator with the default 2 node / 4 guest topology.
func New(log *slog.Logger) *Sim {
	if log == nil {
		log = slog.Default()
	}
	s := &Sim{
		log:      log,
		nodes:    []string{"pve1", "pve2"},
		vms:      map[int]*simVM{},
		imported: map[string][]byte{},
		tasks:    map[string]string{},
	}
	type def struct {
		vmid  int
		name  string
		node  string
		tags  string
		disks []string
	}
	defs := []def{
		{100, "web-01", "pve1", "prod;web", []string{"scsi0", "scsi1"}},
		{101, "db-01", "pve1", "prod;db", []string{"scsi0"}},
		{102, "app-01", "pve2", "dev", []string{"scsi0"}},
		{103, "mail-01", "pve2", "dev;mail", []string{"scsi0"}},
	}
	for _, d := range defs {
		vm := &simVM{
			VMID:      d.vmid,
			Name:      d.name,
			Node:      d.node,
			Status:    "running",
			Tags:      d.tags,
			MaxMem:    2 << 30,
			Uptime:    int64(3600 * (d.vmid - 99)),
			DiskNames: d.disks,
			Content:   map[string][]byte{},
			Snapshots: map[string]map[string][]byte{},
			MutGen:    map[string]uint64{},
		}
		for i, disk := range d.disks {
			buf := make([]byte, DiskSize)
			fillDeterministic(buf, seedFor(d.vmid, i, 0))
			vm.Content[disk] = buf
		}
		s.vms[d.vmid] = vm
	}
	return s
}

// Nodes returns the simulated node names.
func (s *Sim) Nodes() []string { return append([]string(nil), s.nodes...) }

// DiskBytes returns a copy of a guest's current disk content.
func (s *Sim) DiskBytes(vmid int, disk string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.vms[vmid]
	if !ok {
		return nil, false
	}
	b, ok := vm.Content[disk]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), b...), true
}

// ImportedBytes returns a copy of the bytes accepted by proxback-import.
func (s *Sim) ImportedBytes(vmid int, disk string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.imported[importKey(vmid, disk)]
	if !ok {
		return nil, false
	}
	return append([]byte(nil), b...), true
}

// Mutate deterministically rewrites a quarter of the chunks of the guest's first
// disk and reports which disk it touched and how many chunks changed.
func (s *Sim) Mutate(vmid int) (disk string, chunks int, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	vm, ok := s.vms[vmid]
	if !ok {
		return "", 0, fmt.Errorf("pvesim: vm %d not found", vmid)
	}
	disk = vm.DiskNames[0]
	buf := vm.Content[disk]
	vm.MutGen[disk]++
	gen := vm.MutGen[disk]
	total := (len(buf) + ChunkSize - 1) / ChunkSize
	n := total / MutateFraction
	if n < 1 {
		n = 1
	}
	for i := 0; i < n; i++ {
		start := i * ChunkSize
		end := start + ChunkSize
		if end > len(buf) {
			end = len(buf)
		}
		fillDeterministic(buf[start:end], seedFor(vmid, i, gen))
	}
	s.log.Info("sim mutate", "vmid", vmid, "disk", disk, "chunks", n, "generation", gen)
	return disk, n, nil
}

// AuthSeen returns the Authorization header values the simulator has observed.
func (s *Sim) AuthSeen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.authSeen...)
}

func importKey(vmid int, disk string) string { return strconv.Itoa(vmid) + "/" + disk }

// ---------------------------------------------------------------- content gen

func seedFor(vmid, diskIndex int, generation uint64) uint64 {
	return uint64(vmid)*1_000_003 + uint64(diskIndex)*7919 + generation*104_729 + 0x9E3779B97F4A7C15
}

// fillDeterministic writes a reproducible pseudo-random byte sequence.
func fillDeterministic(dst []byte, seed uint64) {
	x := seed
	for i := 0; i < len(dst); i += 8 {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		z ^= z >> 31
		for j := 0; j < 8 && i+j < len(dst); j++ {
			dst[i+j] = byte(z >> (8 * uint(j)))
		}
	}
}

// ---------------------------------------------------------------- HTTP

func (s *Sim) recordAuth(r *http.Request) {
	h := r.Header.Get("Authorization")
	if h == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, seen := range s.authSeen {
		if seen == h {
			return
		}
	}
	s.authSeen = append(s.authSeen, h)
}

func writeData(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "errors": msg})
}

// Handler returns the simulator's HTTP handler.
func (s *Sim) Handler() http.Handler {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			s.recordAuth(req)
			next.ServeHTTP(w, req)
		})
	})

	r.Get("/api2/json/nodes", s.handleNodes)
	r.Get("/api2/json/nodes/{node}/qemu", s.handleQemuList)
	r.Get("/api2/json/nodes/{node}/qemu/{vmid}/config", s.handleConfig)
	r.Post("/api2/json/nodes/{node}/qemu/{vmid}/snapshot", s.handleSnapshotCreate)
	r.Delete("/api2/json/nodes/{node}/qemu/{vmid}/snapshot/{snapname}", s.handleSnapshotDelete)
	r.Get("/api2/json/nodes/{node}/tasks/{upid}/status", s.handleTaskStatus)
	r.Get("/api2/json/nodes/{node}/qemu/{vmid}/proxback-export/{disk}", s.handleExport)
	r.Post("/api2/json/nodes/{node}/qemu/{vmid}/proxback-import/{disk}", s.handleImport)

	r.Get("/sim/disk/{vmid}/{disk}", s.handleSimDisk)
	r.Get("/sim/imported/{vmid}/{disk}", s.handleSimImported)
	r.Post("/sim/mutate/{vmid}", s.handleSimMutate)
	r.Get("/sim/auth", s.handleSimAuth)
	return r
}

func (s *Sim) handleNodes(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, n := range s.Nodes() {
		out = append(out, map[string]any{
			"node": n, "status": "online", "type": "node",
			"cpu": 0.05, "maxcpu": 8, "mem": 4 << 30, "maxmem": 32 << 30,
		})
	}
	writeData(w, out)
}

func (s *Sim) lookupVM(w http.ResponseWriter, r *http.Request) (*simVM, bool) {
	vmid, err := strconv.Atoi(chi.URLParam(r, "vmid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad vmid")
		return nil, false
	}
	s.mu.Lock()
	vm, ok := s.vms[vmid]
	s.mu.Unlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "vm not found")
		return nil, false
	}
	return vm, true
}

func (s *Sim) handleQemuList(w http.ResponseWriter, r *http.Request) {
	node := chi.URLParam(r, "node")
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []map[string]any{}
	for _, vmid := range sortedVMIDs(s.vms) {
		vm := s.vms[vmid]
		if vm.Node != node {
			continue
		}
		var maxdisk int64
		for _, d := range vm.DiskNames {
			maxdisk += int64(len(vm.Content[d]))
		}
		out = append(out, map[string]any{
			"vmid": vm.VMID, "name": vm.Name, "status": vm.Status,
			"maxdisk": maxdisk, "maxmem": vm.MaxMem, "uptime": vm.Uptime,
			"tags": vm.Tags,
			"cpus": 2, "diskread": 0, "diskwrite": 0,
		})
	}
	writeData(w, out)
}

func sortedVMIDs(m map[int]*simVM) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func (s *Sim) handleConfig(w http.ResponseWriter, r *http.Request) {
	vm, ok := s.lookupVM(w, r)
	if !ok {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := map[string]any{
		"name":    vm.Name,
		"tags":    vm.Tags,
		"cores":   2,
		"memory":  vm.MaxMem / (1 << 20),
		"ostype":  "l26",
		"scsihw":  "virtio-scsi-pci",
		"boot":    "order=" + vm.DiskNames[0],
		"ide2":    "local:iso/proxback-test.iso,media=cdrom,size=512M",
		"smbios1": "uuid=" + fmt.Sprintf("%08d-0000-0000-0000-000000000000", vm.VMID),
	}
	for i, d := range vm.DiskNames {
		sizeMiB := len(vm.Content[d]) / (1 << 20)
		cfg[d] = fmt.Sprintf("local-lvm:vm-%d-disk-%d,size=%dM", vm.VMID, i, sizeMiB)
	}
	writeData(w, cfg)
}

func (s *Sim) newUPID(node, kind string, vmid int) string {
	s.taskSeq++
	upid := fmt.Sprintf("UPID:%s:%08X:%08X:%08X:%s:%d:root@pam:",
		node, s.taskSeq, s.taskSeq*7, time.Now().Unix(), kind, vmid)
	s.tasks[upid] = node
	return upid
}

func (s *Sim) handleSnapshotCreate(w http.ResponseWriter, r *http.Request) {
	vm, ok := s.lookupVM(w, r)
	if !ok {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeErr(w, http.StatusBadRequest, "bad form")
		return
	}
	snapname := r.FormValue("snapname")
	if snapname == "" {
		snapname = r.URL.Query().Get("snapname")
	}
	if snapname == "" {
		writeErr(w, http.StatusBadRequest, "snapname required")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	snap := map[string][]byte{}
	for _, d := range vm.DiskNames {
		snap[d] = append([]byte(nil), vm.Content[d]...)
	}
	vm.Snapshots[snapname] = snap
	upid := s.newUPID(vm.Node, "qmsnapshot", vm.VMID)
	s.log.Info("sim snapshot created", "vmid", vm.VMID, "snapname", snapname)
	writeData(w, upid)
}

func (s *Sim) handleSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	vm, ok := s.lookupVM(w, r)
	if !ok {
		return
	}
	snapname := chi.URLParam(r, "snapname")
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(vm.Snapshots, snapname)
	upid := s.newUPID(vm.Node, "qmdelsnapshot", vm.VMID)
	writeData(w, upid)
}

func (s *Sim) handleTaskStatus(w http.ResponseWriter, r *http.Request) {
	upid := chi.URLParam(r, "upid")
	s.mu.Lock()
	_, known := s.tasks[upid]
	s.mu.Unlock()
	if !known {
		writeErr(w, http.StatusNotFound, "no such task")
		return
	}
	writeData(w, map[string]any{
		"upid": upid, "status": "stopped", "exitstatus": "OK",
		"type": "qmsnapshot", "node": chi.URLParam(r, "node"), "user": "root@pam",
	})
}

func (s *Sim) handleExport(w http.ResponseWriter, r *http.Request) {
	vm, ok := s.lookupVM(w, r)
	if !ok {
		return
	}
	disk := chi.URLParam(r, "disk")
	snapshot := r.URL.Query().Get("snapshot")
	s.mu.Lock()
	var data []byte
	if snapshot != "" {
		snap, sok := vm.Snapshots[snapshot]
		if !sok {
			s.mu.Unlock()
			writeErr(w, http.StatusNotFound, "no such snapshot")
			return
		}
		data = snap[disk]
	} else {
		data = vm.Content[disk]
	}
	if data == nil {
		s.mu.Unlock()
		writeErr(w, http.StatusNotFound, "no such disk")
		return
	}
	out := append([]byte(nil), data...)
	s.mu.Unlock()

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func (s *Sim) handleImport(w http.ResponseWriter, r *http.Request) {
	vmid, err := strconv.Atoi(chi.URLParam(r, "vmid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad vmid")
		return
	}
	node := chi.URLParam(r, "node")
	disk := chi.URLParam(r, "disk")
	data, err := io.ReadAll(io.LimitReader(r.Body, 4<<30))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	s.mu.Lock()
	// Like qmrestore, importing into a free VMID creates the guest. The
	// restored VM appears in the inventory as stopped.
	vm, ok := s.vms[vmid]
	if !ok {
		vm = &simVM{
			VMID:      vmid,
			Name:      fmt.Sprintf("restored-%d", vmid),
			Node:      node,
			Status:    "stopped",
			MaxMem:    2 << 30,
			Content:   map[string][]byte{},
			Snapshots: map[string]map[string][]byte{},
			MutGen:    map[string]uint64{},
		}
		s.vms[vmid] = vm
	}
	if _, exists := vm.Content[disk]; !exists {
		vm.DiskNames = append(vm.DiskNames, disk)
	}
	vm.Content[disk] = append([]byte(nil), data...)
	s.imported[importKey(vmid, disk)] = data
	upid := s.newUPID(vm.Node, "qmrestore", vmid)
	s.mu.Unlock()
	s.log.Info("sim import accepted", "vmid", vmid, "disk", disk, "bytes", len(data))
	writeData(w, upid)
}

// ---------------------------------------------------------------- sim-only

func (s *Sim) handleSimDisk(w http.ResponseWriter, r *http.Request) {
	vmid, err := strconv.Atoi(chi.URLParam(r, "vmid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad vmid")
		return
	}
	data, ok := s.DiskBytes(vmid, chi.URLParam(r, "disk"))
	if !ok {
		writeErr(w, http.StatusNotFound, "no such disk")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (s *Sim) handleSimImported(w http.ResponseWriter, r *http.Request) {
	vmid, err := strconv.Atoi(chi.URLParam(r, "vmid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad vmid")
		return
	}
	data, ok := s.ImportedBytes(vmid, chi.URLParam(r, "disk"))
	if !ok {
		writeErr(w, http.StatusNotFound, "nothing imported for that disk")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (s *Sim) handleSimMutate(w http.ResponseWriter, r *http.Request) {
	vmid, err := strconv.Atoi(chi.URLParam(r, "vmid"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad vmid")
		return
	}
	disk, chunks, err := s.Mutate(vmid)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"vmid": vmid, "disk": disk, "chunksChanged": chunks, "chunkSize": ChunkSize,
	})
}

func (s *Sim) handleSimAuth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"tokens": s.AuthSeen()})
}

// Describe returns a human readable topology summary for startup logs.
func (s *Sim) Describe() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "nodes=%s guests=", strings.Join(s.nodes, ","))
	for i, vmid := range sortedVMIDs(s.vms) {
		if i > 0 {
			b.WriteString(",")
		}
		vm := s.vms[vmid]
		fmt.Fprintf(&b, "%d(%s@%s,%d disks)", vm.VMID, vm.Name, vm.Node, len(vm.DiskNames))
	}
	return b.String()
}
